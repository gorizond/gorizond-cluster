package controllers

import (
	"testing"
	"time"

	managementv1 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeIDFromHeadscaleList(t *testing.T) {
	out := "ID\t\tNAME\t\tADDRESS\nabcd123\tnode-1\t10.0.0.2\n" +
		"efgh456\tnode-2\t10.0.0.3\n"

	id := nodeIDFromHeadscaleList(out, "node-2")
	if id != "efgh456" {
		t.Fatalf("expected id efgh456, got %q", id)
	}
}

func TestIsCreateLikeEvent(t *testing.T) {
	n := &managementv1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", CreationTimestamp: metav1.Now()}}
	if !isCreateLikeEvent(n) {
		t.Fatal("expected create-like event for fresh node")
	}

	old := time.Now().Add(-headscaleNodeCreateEventMaxAge - time.Minute)
	n2 := &managementv1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", CreationTimestamp: metav1.NewTime(old)}}
	if isCreateLikeEvent(n2) {
		t.Fatal("expected non create-like event for old node")
	}
}
