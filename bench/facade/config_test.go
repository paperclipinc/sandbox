package main

import (
	"testing"
	"time"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestParseConfigRequiresKubeconfig: the harness refuses to run without a
// cluster target (it measures against a live cluster, there is no default).
func TestParseConfigRequiresKubeconfig(t *testing.T) {
	if _, err := parseConfig(nil); err == nil {
		t.Fatal("parseConfig(nil) = nil error, want a required-kubeconfig error")
	}
}

// TestParseConfigDefaults: with a kubeconfig set, the remaining flags default to
// the facade-conformance fixture (default namespace + default pool) so the
// harness runs against the same objects the CI deploys.
func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]string{"--kubeconfig", "/tmp/kc"})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.namespace != "default" {
		t.Errorf("namespace = %q, want default", cfg.namespace)
	}
	if cfg.pool != "default" {
		t.Errorf("pool = %q, want default", cfg.pool)
	}
	if cfg.iterations != 20 {
		t.Errorf("iterations = %d, want 20", cfg.iterations)
	}
	if cfg.timeout != 60*time.Second {
		t.Errorf("timeout = %s, want 60s", cfg.timeout)
	}
}

// TestParseConfigRejectsZeroIterations: zero iterations would produce an empty
// distribution; reject it rather than print a meaningless summary.
func TestParseConfigRejectsZeroIterations(t *testing.T) {
	if _, err := parseConfig([]string{"--kubeconfig", "/tmp/kc", "--iterations", "0"}); err == nil {
		t.Fatal("parseConfig(iterations=0) = nil error, want a validation error")
	}
}

// TestSandboxReadyPredicate: sandboxReady is True only when the Sandbox carries
// a Ready=True condition (the bare-metal in-VM tail the kind run never reaches).
func TestSandboxReadyPredicate(t *testing.T) {
	cases := []struct {
		name string
		cond *metav1.Condition
		want bool
	}{
		{"no condition", nil, false},
		{"ready false", &metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse}, false},
		{"ready true", &metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := &agentsv1alpha1.Sandbox{}
			if tc.cond != nil {
				sb.Status.Conditions = []metav1.Condition{*tc.cond}
			}
			if got := sandboxReady(sb); got != tc.want {
				t.Errorf("sandboxReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSandboxSpecBound: the Sandbox the harness applies carries the
// agentrun.dev/pool bridge annotation so the facade binds it to the configured
// pool (not the facade default).
func TestSandboxSpecBound(t *testing.T) {
	h := &harness{cfg: config{name: "n", namespace: "ns", pool: "p", image: "busybox:stable"}}
	sb := h.sandbox(1)
	if sb.Annotations[poolAnnotation] != "p" {
		t.Errorf("bridge annotation = %q, want p", sb.Annotations[poolAnnotation])
	}
	if sb.Spec.Replicas == nil || *sb.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", sb.Spec.Replicas)
	}
	if len(sb.Spec.PodTemplate.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(sb.Spec.PodTemplate.Spec.Containers))
	}
	if sb.Spec.PodTemplate.Spec.Containers[0].Image != "busybox:stable" {
		t.Errorf("image = %q, want busybox:stable", sb.Spec.PodTemplate.Spec.Containers[0].Image)
	}
}
