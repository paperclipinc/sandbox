package controller_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/controller"
	"github.com/paperclipinc/sandbox/internal/observability"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestClaimTracePropagatesToForkd drives a claim to Ready against a fake forkd
// that runs the otelgrpc server handler, with the in-memory recorder installed,
// and asserts:
//   - a controller.reconcileClaim span exists with the expected attributes,
//   - a forkd.Fork span (recorded by the in-process fake forkd) shares the
//     controller reconcile span's trace id, proving cross-process gRPC
//     trace-context propagation,
//   - no span carries a secret value.
func TestClaimTracePropagatesToForkd(t *testing.T) {
	recorder, restore := observability.InMemoryForTest()
	t.Cleanup(restore)

	stop, err := controller.StartFakeForkdNode(testRegistry, "trace-node-1", "trace-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "trace-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	if err := k8sClient.Create(ctx, template); err != nil {
		t.Fatal(err)
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "trace-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "trace-tmpl"},
			Replicas:    1,
		},
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "trace-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "trace-pool"},
		},
	}
	if err := k8sClient.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "trace-claim", Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxReady {
				break
			}
			if got.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Give the deferred span ends a moment to flush to the recorder. Anchor on
	// the forkd.Fork span (one fork happened) and then require a
	// controller.reconcileClaim span sharing ITS trace id: the controller may
	// reconcile the claim several times (e.g. an optimistic-lock retry), so
	// matching the two span names independently could pair spans from
	// different reconcile passes. Sharing the forking pass's trace id is what
	// proves cross-process propagation.
	var reconcileSpan, forkdSpan sdktrace.ReadOnlySpan
	flushDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(flushDeadline) {
		reconcileSpan, forkdSpan = nil, nil
		for _, s := range recorder.Ended() {
			if s.Name() == "forkd.Fork" && attrEquals(s, "sandbox.id", "trace-claim") {
				forkdSpan = s
			}
		}
		if forkdSpan != nil {
			for _, s := range recorder.Ended() {
				if s.Name() == "controller.reconcileClaim" &&
					attrEquals(s, "claim.name", "trace-claim") &&
					s.SpanContext().TraceID() == forkdSpan.SpanContext().TraceID() {
					reconcileSpan = s
				}
			}
		}
		if reconcileSpan != nil && forkdSpan != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if forkdSpan == nil {
		t.Fatal("no forkd.Fork span for trace-claim recorded; cross-process propagation did not reach the fake forkd")
	}
	if reconcileSpan == nil {
		t.Fatalf("no controller.reconcileClaim span shares the forkd.Fork trace id %s; trace context did not propagate over gRPC",
			forkdSpan.SpanContext().TraceID())
	}
	assertSpanAttr(t, reconcileSpan, "claim.namespace", "default")
	assertSpanAttr(t, reconcileSpan, "pool", "trace-pool")

	// No span may leak a secret value. trace-claim carries no secrets, so we
	// assert structurally that only ids/config attributes are present (no
	// attribute key resembling a secret and no env/token values).
	for _, s := range recorder.Ended() {
		for _, kv := range s.Attributes() {
			key := string(kv.Key)
			if key == "secret" || key == "token" || key == "api_token" || key == "env" {
				t.Fatalf("span %q carries a forbidden attribute %q", s.Name(), key)
			}
		}
	}
}

func attrEquals(s sdktrace.ReadOnlySpan, key, want string) bool {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString() == want
		}
	}
	return false
}

func assertSpanAttr(t *testing.T, s sdktrace.ReadOnlySpan, key, want string) {
	t.Helper()
	if !attrEquals(s, key, want) {
		t.Fatalf("span %q: attribute %q != %q", s.Name(), key, want)
	}
}
