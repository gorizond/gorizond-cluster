package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorizond/gorizond-cluster/pkg"
	"github.com/rancher/lasso/pkg/log"
	"github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	controllersManagementv3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	controllersProvisionv1 "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	controllersCore "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/v3/pkg/kubeconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const headscaleNodeApprovalLabel = "gorizond-headscale-approve"

// We only want to start processing on "fresh" Node objects, otherwise controller restarts would
// try to approve all historical nodes.
const headscaleNodeCreateEventMaxAge = 10 * time.Minute

var (
	nextRunMutex sync.Mutex
	nextRun      = make(map[string]time.Time)
)

func InitHeadscaleNodeApproveController(
	ctx context.Context,
	NodeResourceController controllersManagementv3.NodeController,
	ProvisionResourceController controllersProvisionv1.ClusterController,
	SecretResourceController controllersCore.SecretController,
) {
	NodeResourceController.OnChange(ctx, "headscale-node-approval", func(key string, node *v3.Node) (*v3.Node, error) {
		if node == nil {
			cleanupPending(key)
			return nil, nil
		}
		if node.DeletionTimestamp != nil {
			cleanupPending(key)
			return node, nil
		}

		// Filter out system clusters
		if node.Namespace == "fleet-local" || node.Namespace == "fleet-default" {
			return node, nil
		}

		// Already approved
		if node.Labels != nil && node.Labels[headscaleNodeApprovalLabel] == "true" {
			cleanupPending(key)
			return node, nil
		}

		// Start only on create-like events; allow retries for keys already marked as pending.
		if !isPending(key) && !isCreateLikeEvent(node) {
			return node, nil
		}

		now := time.Now()
		if wait, ok := shouldThrottle(key, now); ok {
			NodeResourceController.EnqueueAfter(node.Namespace, node.Name, wait)
			return node, nil
		}
		setNextRun(key, now.Add(30*time.Second))

		runtimeClusters, err := listRuntimeClusters(ProvisionResourceController)
		if err != nil {
			log.Errorf("headscale approve: list runtime clusters failed: %v", err)
			setNextRun(key, now.Add(30*time.Second))
			NodeResourceController.EnqueueAfter(node.Namespace, node.Name, 30*time.Second)
			return node, nil
		}

		var (
			cfg              *rest.Config
			clientset        *kubernetes.Clientset
			headscalePodName string
			listOutput       string
			usedRuntime      string
			lastErr          error
		)

		candidates := candidateNodeIdentifiers(node)

		for _, runtimeCluster := range runtimeClusters {
			cfgCandidate, clientCandidate, err := buildRuntimeClientForCluster(SecretResourceController, runtimeCluster)
			if err != nil {
				lastErr = err
				log.Infof("headscale approve: skip runtime cluster %s: build client failed: %v", runtimeCluster, err)
				continue
			}

			podName, err := getHeadscalePodName(ctx, clientCandidate, node.Namespace)
			if err != nil {
				lastErr = err
				log.Infof("headscale approve: skip runtime cluster %s: no ready headscale pod in namespace %s: %v", runtimeCluster, node.Namespace, err)
				continue
			}

			out, _, err := runHeadscaleCommand(cfgCandidate, clientCandidate, node.Namespace, podName, "headscale", "nodes", "list")
			if err != nil {
				lastErr = err
				log.Infof("headscale approve: skip runtime cluster %s: headscale nodes list failed: %v", runtimeCluster, err)
				continue
			}

			cfg = cfgCandidate
			clientset = clientCandidate
			headscalePodName = podName
			listOutput = out
			usedRuntime = runtimeCluster
			break
		}

		if cfg == nil || clientset == nil || headscalePodName == "" {
			if lastErr != nil {
				log.Infof("headscale approve pending: headscale not ready in any runtime cluster for %s/%s: lastErr=%v", node.Namespace, node.Name, lastErr)
			} else {
				log.Infof("headscale approve pending: headscale not ready in any runtime cluster for %s/%s", node.Namespace, node.Name)
			}
			setNextRun(key, now.Add(30*time.Second))
			NodeResourceController.EnqueueAfter(node.Namespace, node.Name, 30*time.Second)
			return node, nil
		}

		log.Infof("headscale approve: selected runtime cluster %s for %s/%s", usedRuntime, node.Namespace, node.Name)

		if !isNodeListed(listOutput, candidates...) {
			setNextRun(key, now.Add(20*time.Second))
			NodeResourceController.EnqueueAfter(node.Namespace, node.Name, 20*time.Second)
			log.Infof("headscale approve pending: node %s/%s not found in headscale list yet", node.Namespace, node.Name)
			return node, nil
		}

		approved := false
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if _, _, err := runHeadscaleCommand(cfg, clientset, node.Namespace, headscalePodName, "headscale", "nodes", "approve", candidate); err == nil {
				approved = true
				break
			}
		}

		if !approved {
			nodeID := nodeIDFromHeadscaleList(listOutput, candidates...)
			if nodeID == "" {
				setNextRun(key, now.Add(30*time.Second))
				NodeResourceController.EnqueueAfter(node.Namespace, node.Name, 30*time.Second)
				log.Errorf("headscale approve failed for %s/%s: no matching node id in list", node.Namespace, node.Name)
				return node, nil
			}
			if _, _, err := runHeadscaleCommand(cfg, clientset, node.Namespace, headscalePodName, "headscale", "nodes", "approve", nodeID); err != nil {
				setNextRun(key, now.Add(30*time.Second))
				NodeResourceController.EnqueueAfter(node.Namespace, node.Name, 30*time.Second)
				log.Errorf("headscale approve failed for %s/%s with id %s: %v", node.Namespace, node.Name, nodeID, err)
				return node, nil
			}
			approved = true
		}

		if !approved {
			setNextRun(key, now.Add(30*time.Second))
			NodeResourceController.EnqueueAfter(node.Namespace, node.Name, 30*time.Second)
			return node, nil
		}

		nodeCopy := node.DeepCopy()
		if nodeCopy.Labels == nil {
			nodeCopy.Labels = map[string]string{}
		}
		nodeCopy.Labels[headscaleNodeApprovalLabel] = "true"
		updated, err := NodeResourceController.Update(nodeCopy)
		if err != nil {
			// Don't fail hard, just retry later.
			log.Errorf("headscale approve: failed to set label for %s/%s: %v", node.Namespace, node.Name, err)
			setNextRun(key, now.Add(30*time.Second))
			NodeResourceController.EnqueueAfter(node.Namespace, node.Name, 30*time.Second)
			return node, nil
		}

		cleanupPending(key)
		log.Infof("headscale node approved and labeled: %s/%s", updated.Namespace, updated.Name)
		return updated, nil
	})
}

func cleanupPending(key string) {
	nextRunMutex.Lock()
	delete(nextRun, key)
	nextRunMutex.Unlock()
}

func isPending(key string) bool {
	nextRunMutex.Lock()
	_, ok := nextRun[key]
	nextRunMutex.Unlock()
	return ok
}

func shouldThrottle(key string, now time.Time) (time.Duration, bool) {
	nextRunMutex.Lock()
	nextExec, exists := nextRun[key]
	nextRunMutex.Unlock()
	if !exists {
		return 0, false
	}
	if now.Before(nextExec) {
		return nextExec.Sub(now), true
	}
	return 0, false
}

func setNextRun(key string, t time.Time) {
	nextRunMutex.Lock()
	nextRun[key] = t
	nextRunMutex.Unlock()
}

func isCreateLikeEvent(node *v3.Node) bool {
	if node == nil {
		return false
	}
	if node.CreationTimestamp.IsZero() {
		// If Rancher doesn't set it for some reason, allow processing.
		return true
	}
	age := time.Since(node.CreationTimestamp.Time)
	if age < 0 {
		age = -age
	}
	return age <= headscaleNodeCreateEventMaxAge
}

func listRuntimeClusters(ProvisionResourceController controllersProvisionv1.ClusterController) ([]string, error) {
	clusters, err := ProvisionResourceController.List("fleet-default", metav1.ListOptions{LabelSelector: "gorizond.runtime=true"})
	if err != nil {
		return nil, fmt.Errorf("failed to list runtime clusters: %w", err)
	}
	if len(clusters.Items) == 0 {
		return nil, fmt.Errorf("no runtime clusters with label gorizond.runtime=true found")
	}

	names := make([]string, 0, len(clusters.Items))
	for _, c := range clusters.Items {
		names = append(names, c.Name)
	}
	return names, nil
}

func buildRuntimeClientForCluster(SecretResourceController controllersCore.SecretController, runtimeCluster string) (*rest.Config, *kubernetes.Clientset, error) {
	secret, err := SecretResourceController.Get("fleet-default", runtimeCluster+"-kubeconfig", metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("runtime kubeconfig secret %s-kubeconfig not found: %w", runtimeCluster, err)
	}

	var cfg *rest.Config
	// Backward compatible: keep the old misspelled env var too.
	debugKubeconfig := os.Getenv("DEBUG_LOCAL_KUBECONFIG")
	if debugKubeconfig == "" {
		debugKubeconfig = os.Getenv("DEBUG_LOCAL_KUBECOFING")
	}
	if debugKubeconfig != "" {
		cfg, err = kubeconfig.GetNonInteractiveClientConfig(debugKubeconfig).ClientConfig()
	} else {
		cfg, err = pkg.GetRestConfig(secret.Data["value"])
	}
	if err != nil {
		return nil, nil, err
	}

	clientset, err := pkg.CreateClientset(cfg)
	if err != nil {
		return nil, nil, err
	}

	return cfg, clientset, nil
}

func getHeadscalePodName(ctx context.Context, clientset *kubernetes.Clientset, namespace string) (string, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=headscale"})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no headscale pods found")
	}

	for _, pod := range pods.Items {
		if len(pod.Status.ContainerStatuses) == 0 {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "headscale" && status.Ready {
				return pod.Name, nil
			}
		}
	}

	return "", fmt.Errorf("headscale pods found in namespace %s but no ready container", namespace)
}

func runHeadscaleCommand(cfg *rest.Config, clientset *kubernetes.Clientset, namespace, podName string, command ...string) (string, string, error) {
	return pkg.ExecCommand(cfg, clientset, namespace, podName, "headscale", command)
}

func candidateNodeIdentifiers(node *v3.Node) []string {
	ids := []string{}
	if node.Name != "" {
		ids = append(ids, node.Name)
	}
	if node.Status.NodeName != "" && node.Status.NodeName != node.Name {
		ids = append(ids, node.Status.NodeName)
	}
	return ids
}

func isNodeListed(listOutput string, candidates ...string) bool {
	if len(candidates) == 0 {
		return false
	}
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(listOutput, candidate) {
			return true
		}
	}
	return false
}

func nodeIDFromHeadscaleList(listOutput string, candidates ...string) string {
	head := map[string]struct{}{"ID": {}, "NAME": {}, "NODE_NAME": {}}
	for _, line := range strings.Split(listOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if _, isHeader := head[strings.ToUpper(fields[0])]; isHeader {
			continue
		}
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			for _, field := range fields {
				if field == candidate {
					return fields[0]
				}
			}
		}
	}
	return ""
}
