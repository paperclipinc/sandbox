package controller

import (
	"context"
	"net"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeInfoFromPod(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "forkd-abc12",
			Labels: map[string]string{"app.kubernetes.io/component": "forkd"},
		},
		Spec:   corev1.PodSpec{NodeName: "worker-1"},
		Status: corev1.PodStatus{PodIP: "10.0.3.7", Phase: corev1.PodRunning},
	}

	info, ok := NodeInfoFromPod(pod, 9090, 9091, 9092)
	if !ok {
		t.Fatal("expected ok")
	}
	if info.Name != "worker-1" {
		t.Fatalf("name = %q, want worker-1", info.Name)
	}
	if info.Endpoint != "10.0.3.7:9090" {
		t.Fatalf("endpoint = %q", info.Endpoint)
	}
	if info.HTTPEndpoint != "10.0.3.7:9091" {
		t.Fatalf("httpEndpoint = %q", info.HTTPEndpoint)
	}
	// The CAS endpoint is the same pod IP with the dedicated CAS port, a SEPARATE
	// port from the sandbox HTTP API.
	if info.CASEndpoint != "10.0.3.7:9092" {
		t.Fatalf("casEndpoint = %q, want 10.0.3.7:9092", info.CASEndpoint)
	}
}

func TestNodeInfoFromPodSkipsNotReady(t *testing.T) {
	for _, pod := range []corev1.Pod{
		{Status: corev1.PodStatus{PodIP: "", Phase: corev1.PodRunning}, Spec: corev1.PodSpec{NodeName: "w"}},
		{Status: corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodPending}, Spec: corev1.PodSpec{NodeName: "w"}},
		{Status: corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning}, Spec: corev1.PodSpec{NodeName: ""}},
	} {
		if _, ok := NodeInfoFromPod(pod, 9090, 9091, 9092); ok {
			t.Fatalf("expected not ok for pod %+v", pod.Status)
		}
	}
}

// TestSyncPodsRegistersAndRefreshesCapacity drives syncPods against a live
// fake forkd. The pod's PodIP is 127.0.0.1 and GRPCPort is the fake server's
// port so the derived Endpoint matches the listening address.
func TestSyncPodsRegistersAndRefreshesCapacity(t *testing.T) {
	addr, _ := startFakeForkd(t, "disc-tmpl")
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	registry := NewNodeRegistry()
	d := &ForkdDiscovery{
		Registry: registry,
		GRPCPort: port,
		HTTPPort: port + 1,
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "forkd-xyz99",
			Labels: map[string]string{"app.kubernetes.io/component": "forkd"},
		},
		Spec:   corev1.PodSpec{NodeName: "disc-worker"},
		Status: corev1.PodStatus{PodIP: host, Phase: corev1.PodRunning},
	}

	d.syncPods(context.Background(), []corev1.Pod{pod})

	node, ok := registry.GetNode("disc-worker")
	if !ok {
		t.Fatal("node not registered")
	}
	if node.Endpoint != addr {
		t.Fatalf("endpoint = %q, want %q", node.Endpoint, addr)
	}
	if len(node.TemplateIDs) != 1 || node.TemplateIDs[0] != "disc-tmpl" {
		t.Fatalf("templateIDs = %v, want [disc-tmpl] (capacity not refreshed)", node.TemplateIDs)
	}
	if node.MaxSandboxes == 0 {
		t.Fatal("maxSandboxes not refreshed from GetCapacity")
	}
}
