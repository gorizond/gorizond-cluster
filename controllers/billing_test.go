package controllers

import (
	"testing"
	"time"

	managementv1 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExtraFreeClusterWorkersKeepsOldestWithinLimit(t *testing.T) {
	now := time.Now()
	nodes := []*managementv1.Node{
		newTestNode("w1", true, false, false, now.Add(-4*time.Hour)),
		newTestNode("w2", true, false, false, now.Add(-3*time.Hour)),
		newTestNode("w3", true, false, false, now.Add(-2*time.Hour)),
		newTestNode("w4", true, false, false, now.Add(-1*time.Hour)),
	}

	extra := extraFreeClusterWorkers(nodes, 2)

	want := []string{"w3", "w4"}
	if len(extra) != len(want) {
		t.Fatalf("expected %d extra workers, got %d", len(want), len(extra))
	}
	for i, node := range extra {
		if node.Name != want[i] {
			t.Fatalf("expected extra worker %s at position %d, got %s", want[i], i, node.Name)
		}
	}
}

func TestExtraFreeClusterWorkersZeroLimitDeletesAllWorkers(t *testing.T) {
	now := time.Now()
	nodes := []*managementv1.Node{
		newTestNode("w1", true, false, false, now.Add(-2*time.Hour)),
		newTestNode("w2", true, false, false, now.Add(-1*time.Hour)),
	}

	extra := extraFreeClusterWorkers(nodes, 0)

	want := []string{"w1", "w2"}
	if len(extra) != len(want) {
		t.Fatalf("expected %d extra workers, got %d", len(want), len(extra))
	}
	for i, node := range extra {
		if node.Name != want[i] {
			t.Fatalf("expected %s at %d, got %s", want[i], i, node.Name)
		}
	}
}

func TestExtraFreeClusterWorkersIgnoresNonWorkers(t *testing.T) {
	now := time.Now()
	nodes := []*managementv1.Node{
		newTestNode("cp1", false, true, false, now.Add(-3*time.Hour)),
		newTestNode("etcd1", false, false, true, now.Add(-2*time.Hour)),
		newTestNode("w1", true, false, false, now.Add(-2*time.Hour)),
		newTestNode("w2", true, false, false, now.Add(-1*time.Hour)),
	}

	extra := extraFreeClusterWorkers(nodes, 1)

	if len(extra) != 1 {
		t.Fatalf("expected 1 extra worker, got %d", len(extra))
	}
	if extra[0].Name != "w2" {
		t.Fatalf("expected to delete newest worker w2, got %s", extra[0].Name)
	}
}

func newTestNode(name string, worker, controlPlane, etcd bool, created time.Time) *managementv1.Node {
	return &managementv1.Node{
		ObjectMeta: v1.ObjectMeta{
			Name:              name,
			Namespace:         "test-cluster",
			CreationTimestamp: v1.NewTime(created),
		},
		Spec: managementv1.NodeSpec{
			Worker:       worker,
			ControlPlane: controlPlane,
			Etcd:         etcd,
		},
	}
}
