package controllers

import (
	"context"
	"os"
	"strconv"
	"time"

	provv1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
	provcontrollers "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io/v1"
	"github.com/rancher/lasso/pkg/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultBillingRate = 0.015
)

// InitPeriodicBillingController initializes the controller for periodic creation of BillingEvent.
func InitPeriodicBillingController(ctx context.Context, clusterController provcontrollers.ClusterController, billingEventController provcontrollers.BillingEventController) {
	// Get the rate per minute from the environment variable
	ratePerMinuteStr := os.Getenv("BILLING_RATE_PER_MINUTE")
	ratePerMinute := defaultBillingRate
	if ratePerMinuteStr != "" {
		var err error
		ratePerMinute, err = strconv.ParseFloat(ratePerMinuteStr, 64)
		if err != nil {
			log.Errorf("Invalid BILLING_RATE_PER_MINUTE value: %s. Defaulting to %f.", ratePerMinuteStr, defaultBillingRate)
			ratePerMinute = defaultBillingRate
		}
	}

	// Register a change handler for clusters
	clusterController.OnChange(ctx, "periodic-billing-controller", func(key string, cluster *provv1.Cluster) (*provv1.Cluster, error) {
		if cluster == nil || cluster.DeletionTimestamp != nil {
			return nil, nil
		}

		// Check if spec.billing is present
		if cluster.Spec.Billing == "" {
			// If billing is not set, schedule the next check in a minute
			clusterController.EnqueueAfter(cluster.Namespace, cluster.Name, 1*time.Minute)
			return cluster, nil
		}

		// Get the current time
		now := metav1.Now()

		// Check lastTransitionBillingTime
		if cluster.Status.LastTransitionBillingTime.IsZero() {
			// If the time is not set, initialize it with the current time
			clusterCopy := cluster.DeepCopy()
			clusterCopy.Status.LastTransitionBillingTime = now
			updatedCluster, err := clusterController.UpdateStatus(clusterCopy)
			if err != nil {
				log.Errorf("Failed to initialize lastTransitionBillingTime for cluster %s/%s: %v", cluster.Namespace, cluster.Name, err)
				return cluster, err
			}
			// Schedule the next check in a minute
			clusterController.EnqueueAfter(cluster.Namespace, cluster.Name, 1*time.Minute)
			return updatedCluster, nil
		}

		// Calculate the time difference
		timeDiff := now.Sub(cluster.Status.LastTransitionBillingTime.Time)
		minutes := int(timeDiff / time.Minute)

		if minutes >= 1 {
			// Calculate the charge amount
			charge := float64(minutes) * ratePerMinute
			// Generate only for paid clusters
			if cluster.Status.Billing != "free" {
				// Create a BillingEvent
				billingEvent := &provv1.BillingEvent{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: cluster.Name + "-",
						Namespace:    cluster.Namespace,
					},
				}

				// Personally create the BillingEvent
				billingEvent, err := billingEventController.Create(billingEvent)
				if err != nil {
					log.Errorf("Failed to create BillingEvent for cluster %s/%s: %v", cluster.Namespace, cluster.Name, err)
					return cluster, err
				}
				billingEvent.Status = provv1.BillingEventStatus{
					Type:          "usage",
					TransitionTime: now,
					Amount:        -charge, // Negative value for deduction
					BillingName:   cluster.Spec.Billing,
				}
				// Update the event status
				billingEventController.UpdateStatus(billingEvent)
			}
			// Update lastTransitionBillingTime
			newLastTime := cluster.Status.LastTransitionBillingTime.Add(time.Duration(minutes) * time.Minute)
			clusterCopy := cluster.DeepCopy()
			clusterCopy.Status.LastTransitionBillingTime = metav1.Time{Time: newLastTime}
			updatedCluster, err := clusterController.Update(clusterCopy)
			if err != nil {
				log.Errorf("Failed to update lastTransitionBillingTime for cluster %s/%s: %v", cluster.Namespace, cluster.Name, err)
				return cluster, err
			}
			if cluster.Status.Billing != "free" {
				log.Infof("Created BillingEvent for cluster %s/%s with amount %f",
					cluster.Namespace, cluster.Name, -charge)
			}
			cluster = updatedCluster
		}

		// Schedule the next check in a minute
		clusterController.EnqueueAfter(cluster.Namespace, cluster.Name, 1*time.Minute)
		return cluster, nil
	})
}