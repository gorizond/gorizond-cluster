package controllers

import (
	"context"
	"os"
	"strconv"

	gorizondControllers "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	provv1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
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
			return updatedCluster, nil
		}

		return cluster, nil
	})

	// Handler for changes in Billing to reconcile clusters
	BillingController.OnChange(ctx, "billing-change", func(key string, billing *provv1.Billing) (*provv1.Billing, error) {
		if billing == nil {
			return nil, nil
		}

		// Get all clusters in the namespace
		clusters, err := GorizondResourceController.Cache().List(billing.Namespace, labels.Everything())
		if err != nil {
			log.Errorf("Failed to list clusters in namespace %s: %v", billing.Namespace, err)
			return nil, err
		}

		// Reconcile each cluster
		// TODO: Use only clusters attached to this billing
		for _, cluster := range clusters {
			GorizondResourceController.Enqueue(cluster.Namespace, cluster.Name)
			log.Infof("Enqueued cluster %s/%s for reconciliation due to billing change", cluster.Namespace, cluster.Name)
		}

		return nil, nil
	})
}