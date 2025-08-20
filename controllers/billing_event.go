package controllers

import (
	"context"
	"fmt"
	"sort"
	"sync"

	provv1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
	provcontrollers "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io/v1"
	"github.com/rancher/lasso/pkg/log"
	"github.com/rancher/wrangler/v3/pkg/apply"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
)

// billingKey serves as the key for the queue: (namespace, billingName)
type billingKey struct {
	Namespace string
	Name      string
}

type Handler struct {
	ctx       context.Context
	apply     apply.Apply
	events    provcontrollers.BillingEventController
	billings  provcontrollers.BillingController
	queue     workqueue.RateLimitingInterface
	lockMap   *sync.Map
	realClock clock.Clock
}

// Register registers an OnChange-handler for BillingEvent and starts worker pools.
// The number of workers can be adjusted via workerCount.
func InitBillingEventController(ctx context.Context,
	billing provcontrollers.BillingController,
	events provcontrollers.BillingEventController,
	apply apply.Apply,
	workerCount int,
) {
	h := &Handler{
		ctx:       ctx,
		apply:     apply,
		events:    events,
		billings:  billing,
		queue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "billing-queue"),
		lockMap:   &sync.Map{},
		realClock: clock.RealClock{},
	}

	// On any change to BillingEvent, add the key (namespace, "default") to the queue.
	events.OnChange(ctx, "billingevent-handler", h.onEventChange)

	// Start a pool of workerCount workers
	for i := 0; i < workerCount; i++ {
		go func() {
			for h.processNextItem() {
			}
		}()
	}
}

// onEventChange is triggered on each Create/Update BillingEvent.
// If the object is nil or marked for deletion, it is ignored.
// We assume evt.Status.BillingName for evt.Namespace, BillingName.
func (h *Handler) onEventChange(key string, evt *provv1.BillingEvent) (*provv1.BillingEvent, error) {
	if evt == nil || evt.DeletionTimestamp != nil {
		return nil, nil
	}

	// Consider (evt.Namespace, evt.Status.BillingName) as the billing key.
	if evt.Status.BillingName != "" {
		h.queue.Add(billingKey{Namespace: evt.Namespace, Name: evt.Status.BillingName})
	}
	return nil, nil
}

// processNextItem retrieves a key from the queue, gets a mutex for this key,
// calls reconcile, and if there is an error, adds the item back with RateLimit.
// If everything is fine, queue.Forget(item).
func (h *Handler) processNextItem() bool {
	item, shutdown := h.queue.Get()
	if shutdown {
		return false
	}
	defer h.queue.Done(item)

	key := item.(billingKey)

	// Get/create a mutex for (namespace/billingName)
	mutexIface, _ := h.lockMap.LoadOrStore(key, &sync.Mutex{})
	mutex := mutexIface.(*sync.Mutex)

	mutex.Lock()
	defer mutex.Unlock()

	if err := h.reconcile(key); err != nil {
		log.Infof("error processing billing %s/%s: %v\n", key.Namespace, key.Name, err)
		h.queue.AddRateLimited(key)
		return true
	}

	h.queue.Forget(item)
	return true
}

// reconcile — we get the list of all BillingEvent in Namespace, sort by CreationTimestamp,
// skip those already processed (by LastEventId in Billing status), and process exactly one.
func (h *Handler) reconcile(key billingKey) error {
	// 1) Get Billing (assume it is named key.Name)
	billing, err := h.billings.Cache().Get(key.Namespace, key.Name)
	if err != nil {
		return fmt.Errorf("failed to get billing %s/%s: %w", key.Namespace, key.Name, err)
	}

	// 2) List all BillingEvent in this Namespace
	events, err := h.events.Cache().List(key.Namespace, labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list billingevents in %s: %w", key.Namespace, err)
	}

	// 3) Sort all events by CreationTimestamp (oldest first)
	sort.Slice(events, func(i, j int) bool {
		return events[i].CreationTimestamp.Before(&events[j].CreationTimestamp)
	})

	// 4) Filter events addressed to this billing
	filtered := make([]*provv1.BillingEvent, 0, len(events))
	for _, evt := range events {
		if evt.Status.BillingName == billing.Name {
			filtered = append(filtered, evt)
		}
	}

	// 5) Process the first unprocessed event for this billing
	for _, evt := range filtered {
		// Skip if this evt is already processed (same UID as LastEventId)
		if billing.Status.LastEventId == string(evt.UID) {
			continue
		}

		// 5) Adjust the balance: subtract evt.Status.Amount
		//    This works for both positive and negative amounts:
		//      - if amount > 0 → balance decreases
		//      - if amount < 0 → balance increases (subtracting a negative)
		billing.Status.Balance += evt.Status.Amount
		billing.Status.LastChargedAt = metav1.NewTime(h.realClock.Now())
		billing.Status.LastEventId = string(evt.UID)

		_, err := h.billings.UpdateStatus(billing)
		if err != nil {
			return fmt.Errorf("failed to update billing status: %w", err)
		}

		// 6) Delete the BillingEvent because it has been processed
		err = h.events.Delete(evt.Namespace, evt.Name, &metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("failed to delete billingevent %s/%s: %w", evt.Namespace, evt.Name, err)
		}

		// 7) Process exactly one event at a time – exit the loop after successful processing
		break
	}

	return nil
}
