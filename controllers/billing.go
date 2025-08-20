package controllers

import (
	"context"
	"os"
	"strconv"
	"time"

	provv1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
	gorizondControllers "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	"github.com/rancher/lasso/pkg/log"
	managementv1 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	controllersManagement "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func InitBillingClusterController(ctx context.Context, mgmtProvision *controllersManagement.Factory, mgmtGorizond *gorizondControllers.Factory) {
	ProvisionResourceController := mgmtProvision.Management().V3().Cluster()
	GorizondResourceController := mgmtGorizond.Provisioning().V1().Cluster()
	BillingController := mgmtGorizond.Provisioning().V1().Billing()

	const billingFinalizer = "provisioning.gorizond.io/billing-balance-transfer"

	// Finalizer and balance transfer on Billing deletion
	BillingController.OnChange(ctx, "billing-balance-transfer", func(key string, billing *provv1.Billing) (*provv1.Billing, error) {
		if billing == nil {
			return nil, nil
		}

		// Ensure finalizer is added on non-deleting objects
		if billing.DeletionTimestamp == nil {
			has := false
			for _, f := range billing.Finalizers {
				if f == billingFinalizer {
					has = true
					break
				}
			}
			if !has {
				copyObj := billing.DeepCopy()
				copyObj.Finalizers = append(copyObj.Finalizers, billingFinalizer)
				updated, err := BillingController.Update(copyObj)
				if err != nil {
					log.Errorf("Failed to add finalizer to billing %s/%s: %v", billing.Namespace, billing.Name, err)
					return billing, err
				}
				return updated, nil
			}
			return billing, nil
		}

		// Deletion: pre-check that the billing is not used by clusters
		clusters, err := GorizondResourceController.Cache().List(billing.Namespace, labels.Everything())
		if err != nil {
			log.Errorf("Failed to list clusters in namespace %s: %v", billing.Namespace, err)
			return billing, err
		}
		for _, c := range clusters {
			if c.Spec.Billing == billing.Name {
				// Billing is in use — block deletion and keep the finalizer
				log.Errorf("Cannot delete billing %s/%s: it is used by cluster %s/%s", billing.Namespace, billing.Name, c.Namespace, c.Name)
				// Re-enqueue to retry later to avoid indefinite stall
				BillingController.EnqueueAfter(billing.Namespace, billing.Name, 30*time.Second)
				return billing, nil
			}
		}

		// Deletion: transfer balance
		if billing.Status.Balance == 0 {
			// Nothing to transfer — remove the finalizer
			copyObj := billing.DeepCopy()
			newFinalizers := make([]string, 0, len(copyObj.Finalizers))
			for _, f := range copyObj.Finalizers {
				if f != billingFinalizer {
					newFinalizers = append(newFinalizers, f)
				}
			}
			copyObj.Finalizers = newFinalizers
			updated, err := BillingController.Update(copyObj)
			if err != nil {
				log.Errorf("Failed to remove finalizer from billing %s/%s: %v", billing.Namespace, billing.Name, err)
				return billing, err
			}
			return updated, nil
		}

		// Find a target billing in the same namespace
		list, err := BillingController.Cache().List(billing.Namespace, labels.Everything())
		if err != nil {
			log.Errorf("Failed to list billings in namespace %s: %v", billing.Namespace, err)
			return billing, err
		}
		var target *provv1.Billing
		for _, b := range list {
			if b.Name == billing.Name {
				continue
			}
			if b.DeletionTimestamp == nil {
				// Choose deterministically by minimal name
				if target == nil || b.Name < target.Name {
					target = b
				}
			}
		}

		// If no target found, use/create a recovered billing
		if target == nil {
			for _, b := range list {
				if b.DeletionTimestamp == nil && b.Labels != nil && b.Labels["provisioning.gorizond.io/recovered"] == "true" {
					target = b
					break
				}
			}
			if target == nil {
				newBilling := &provv1.Billing{
					ObjectMeta: v1.ObjectMeta{
						GenerateName: "recovered-",
						Namespace:    billing.Namespace,
						Labels: map[string]string{
							"provisioning.gorizond.io/recovered": "true",
						},
					},
				}
				_, err := BillingController.Create(newBilling)
				if err != nil {
					log.Errorf("Failed to create recovered billing in namespace %s: %v", billing.Namespace, err)
					return billing, err
				}
				// Give the cache time to observe the new object and retry
				BillingController.EnqueueAfter(billing.Namespace, billing.Name, 1*time.Second)
				return billing, nil
			}
		}

		// Transfer balance (fetch the latest target version)
		targetCur, err := BillingController.Get(target.Namespace, target.Name, v1.GetOptions{})
		if err != nil {
			log.Errorf("Failed to get target billing %s/%s: %v", target.Namespace, target.Name, err)
			return billing, err
		}
		targetCopy := targetCur.DeepCopy()
		targetCopy.Status.Balance += billing.Status.Balance
		targetCopy.Status.LastChargedAt = v1.Now()
		if targetCopy.Status.LastEventId == "" {
			targetCopy.Status.LastEventId = "transfer"
		}
		if _, err := BillingController.UpdateStatus(targetCopy); err != nil {
			log.Errorf("Failed to update target billing %s/%s status: %v", targetCopy.Namespace, targetCopy.Name, err)
			return billing, err
		}

		// Zero out the source balance (idempotent)
		sourceCopy := billing.DeepCopy()
		sourceCopy.Status.Balance = 0
		sourceCopy.Status.LastChargedAt = v1.Now()
		if _, err := BillingController.UpdateStatus(sourceCopy); err != nil {
			log.Errorf("Failed to zero out source billing %s/%s status: %v", sourceCopy.Namespace, sourceCopy.Name, err)
			return billing, err
		}

		// Remove the finalizer
		copyObj := billing.DeepCopy()
		newFinalizers := make([]string, 0, len(copyObj.Finalizers))
		for _, f := range copyObj.Finalizers {
			if f != billingFinalizer {
				newFinalizers = append(newFinalizers, f)
			}
		}
		copyObj.Finalizers = newFinalizers
		updated, err := BillingController.Update(copyObj)
		if err != nil {
			log.Errorf("Failed to remove finalizer from billing %s/%s after transfer: %v", billing.Namespace, billing.Name, err)
			return billing, err
		}
		return updated, nil
	})

	billingFreeNodeCountStr := os.Getenv("BILLING_FREE_NODE_COUNT")
	if billingFreeNodeCountStr == "" {
		billingFreeNodeCountStr = "1"
	}
	billingFreeNodeCount, err := strconv.Atoi(billingFreeNodeCountStr)
	if err != nil {
		log.Errorf("Invalid BILLING_FREE_NODE_COUNT value: %s. Defaulting to 1.", billingFreeNodeCount)
		billingFreeNodeCount = 1
	}

	ProvisionResourceController.OnChange(ctx, "billing-cluster-controller", func(key string, obj *managementv1.Cluster) (*managementv1.Cluster, error) {
		if obj == nil {
			return nil, nil
		}
		// ignore system clusters
		if obj.Spec.FleetWorkspaceName == "fleet-default" || obj.Spec.FleetWorkspaceName == "fleet-local" {
			return nil, nil
		}

		gorizond, err := GorizondResourceController.Get(obj.Spec.FleetWorkspaceName, obj.Spec.DisplayName, v1.GetOptions{})
		if err != nil {
			return nil, err
		}
		// check free cluster for node count
		if gorizond.Status.Billing == "free" {
			if obj.Status.NodeCount > billingFreeNodeCount {
				log.Infof("Successfully checked cluster %s nodes: %s", obj.Name, obj.Status.NodeCount)
				// TODO: Add scaling or notification logic
				// For example, remove extra nodes or send a notification
			}
			return obj, nil
		}
		return obj, nil
	})

	// Handler for changes in gorizond cluster
	GorizondResourceController.OnChange(ctx, "cluster-billing-controller", func(key string, cluster *provv1.Cluster) (*provv1.Cluster, error) {
		if cluster == nil || cluster.DeletionTimestamp != nil {
			return nil, nil
		}
		billing := &provv1.Billing{}
		// Ignore if the cluster is free or has no billing at all
		if cluster.Spec.Billing == "" {
			billing.Status.Balance = 0
			billing.Name = "free"
		} else {
			// Get the billing resource for the cluster's namespace
			billing, err = BillingController.Get(cluster.Namespace, cluster.Spec.Billing, v1.GetOptions{})
			if err != nil {
				log.Errorf("Failed to get billing for namespace %s: %v", cluster.Namespace, err)
				return cluster, err
			}
		}

		// Determine the billing status based on the balance
		var billingStatus string
		if billing.Status.Balance > 0.0 {
			billingStatus = billing.Name
		} else {
			billingStatus = "free"
		}

		// Update the cluster status if it has changed
		if cluster.Status.Billing != billingStatus {
			clusterCopy := cluster.DeepCopy()
			clusterCopy.Status.Billing = billingStatus
			clusterCopy.Status.LastTransitionBillingTime = v1.Now()
			updatedCluster, err := GorizondResourceController.Update(clusterCopy)
			if err != nil {
				log.Errorf("Failed to update cluster %s status: %v", key, err)
				return cluster, err
			}
			log.Infof("Updated cluster %s billing status to %s", cluster.Name, billingStatus)
			// Enqueue the corresponding billing only when status actually changes
			if updatedCluster.Spec.Billing != "" {
				BillingController.Enqueue(updatedCluster.Namespace, updatedCluster.Spec.Billing)
			}
			return updatedCluster, nil
		}
		return cluster, nil
	})

	// Handler for changes in Billing to reconcile only dependent clusters
	BillingController.OnChange(ctx, "billing-change", func(key string, billing *provv1.Billing) (*provv1.Billing, error) {
		if billing == nil {
			return nil, nil
		}

		// Get only clusters in the namespace and enqueue those attached to this billing
		clusters, err := GorizondResourceController.Cache().List(billing.Namespace, labels.Everything())
		if err != nil {
			log.Errorf("Failed to list clusters in namespace %s: %v", billing.Namespace, err)
			return nil, err
		}

		for _, cluster := range clusters {
			if cluster.Spec.Billing == billing.Name {
				GorizondResourceController.Enqueue(cluster.Namespace, cluster.Name)
				log.Infof("Enqueued cluster %s/%s for reconciliation due to billing change", cluster.Namespace, cluster.Name)
			}
		}

		return nil, nil
	})
}
