package controller_test

// Envtest coverage for the trace-to-revision link (W4 observability slice).
//
// When tracing is enabled, dehydrateOnTerminate starts a workspace.dehydrate
// child span of controller.reconcileClaim and stamps the active trace id onto
// the new WorkspaceRevision via the mitos.run/trace-id annotation. These
// tests assert:
//   - the new revision's mitos.run/trace-id annotation equals the reconcile
//     trace id;
//   - a workspace.dehydrate span exists as a child of the claim reconcile span
//     with the expected content-pointer attributes and no secret values;
//   - with tracing OFF (the no-op provider), the revision carries no annotation
//     (no fake id).

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/observability"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const traceIDAnnotationKey = "mitos.run/trace-id"

// TestDehydrateStampsTraceIDOnRevision drives a claim-with-workspace to terminate
// with the in-memory span recorder installed and asserts the trace id is stamped
// on the new revision and a child workspace.dehydrate span is recorded with the
// expected attributes and no secret values.
func TestDehydrateStampsTraceIDOnRevision(t *testing.T) {
	recorder, restore := observability.InMemoryForTest()
	t.Cleanup(restore)

	rec := &wsRecorder{}
	revDigest := cas.Digest(testManifest(0xce))
	rec.install(t, revDigest)

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-trace-node", "wst-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-bind-trace", v1alpha1.WorkspaceRetention{})

	makeBoundClaim(t, "wst", "ws-bind-trace", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-trace-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "wst-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wst-claim", v1alpha1.SandboxTerminated)

	// The workspace head advanced to the new revision; read it back.
	ws := waitWorkspace(t, "ws-bind-trace", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != "" && ws.Status.Revisions >= 1
	}, "head advanced after dehydrate")

	var head v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: ws.Status.Head}, &head); err != nil {
		t.Fatalf("get head revision: %v", err)
	}

	stamped := head.Annotations[traceIDAnnotationKey]
	if stamped == "" {
		t.Fatal("revision has no mitos.run/trace-id annotation with tracing enabled")
	}

	// Find the workspace.dehydrate span that produced THIS revision, then require
	// it shares the trace id stamped on the revision and is a child of a
	// controller.reconcileClaim span from the same trace. Anchoring on the
	// revision name pairs the span to this revision even if the controller
	// reconciled several times.
	var dehydrateSpan, reconcileSpan sdktrace.ReadOnlySpan
	flushDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(flushDeadline) {
		dehydrateSpan, reconcileSpan = nil, nil
		for _, s := range recorder.Ended() {
			if s.Name() == "workspace.dehydrate" && attrEquals(s, "revision.name", head.Name) {
				dehydrateSpan = s
			}
		}
		if dehydrateSpan != nil {
			for _, s := range recorder.Ended() {
				if s.Name() == "controller.reconcileClaim" &&
					s.SpanContext().SpanID() == dehydrateSpan.Parent().SpanID() &&
					s.SpanContext().TraceID() == dehydrateSpan.SpanContext().TraceID() {
					reconcileSpan = s
				}
			}
		}
		if dehydrateSpan != nil && reconcileSpan != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if dehydrateSpan == nil {
		t.Fatal("no workspace.dehydrate span recorded for the new revision")
	}
	if reconcileSpan == nil {
		t.Fatalf("workspace.dehydrate span is not a child of a controller.reconcileClaim span sharing its trace id %s",
			dehydrateSpan.SpanContext().TraceID())
	}

	// The stamped annotation must equal the dehydrate span's (and thus the
	// reconcile's) trace id.
	if got := dehydrateSpan.SpanContext().TraceID().String(); got != stamped {
		t.Fatalf("revision trace-id annotation %q != reconcile trace id %q", stamped, got)
	}

	// The span carries the expected content-pointer attributes.
	assertSpanAttr(t, dehydrateSpan, "workspace.name", "ws-bind-trace")
	assertSpanAttr(t, dehydrateSpan, "content.manifest.digest", string(revDigest))
	if !attrEquals(dehydrateSpan, "revision.name", head.Name) {
		t.Fatalf("workspace.dehydrate span revision.name != %q", head.Name)
	}
	var sawPathCount, sawPaired bool
	for _, kv := range dehydrateSpan.Attributes() {
		switch string(kv.Key) {
		case "captured.path.count":
			sawPathCount = true
		case "memory.snapshot.paired":
			sawPaired = true
			if kv.Value.AsBool() {
				t.Fatal("memory.snapshot.paired true for a plain terminate")
			}
		}
	}
	if !sawPathCount || !sawPaired {
		t.Fatalf("workspace.dehydrate span missing count/paired attributes (count=%v paired=%v)", sawPathCount, sawPaired)
	}

	// No span may leak a secret value: no attribute key resembling a secret.
	for _, s := range recorder.Ended() {
		for _, kv := range s.Attributes() {
			key := string(kv.Key)
			if key == "secret" || key == "token" || key == "api_token" || key == "env" {
				t.Fatalf("span %q carries a forbidden attribute %q", s.Name(), key)
			}
		}
	}
}

// TestTraceIDAnnotationsOmittedWhenTracingOff asserts the stamp logic omits the
// annotation entirely under the no-op provider (tracing disabled): a no-op span
// context carries an invalid trace id, so no fake all-zero id is written.
//
// This is a deterministic unit test of the controller's stamp helper rather than
// an envtest round trip: the OTel global tracer wires its delegate exactly once
// per process, so once any earlier test installs a recording provider the global
// tracer keeps recording, and a runtime toggle back to no-op via the shared
// manager is not observable in-process. The omit branch is the code under test,
// and a no-op span context exercises it directly and reliably.
func TestTraceIDAnnotationsOmittedWhenTracingOff(t *testing.T) {
	// A context with a no-op span: its span context trace id is invalid, exactly
	// as it is when tracing is disabled (the no-op provider).
	_, span := noop.NewTracerProvider().Tracer("t").Start(context.Background(), "off")
	defer span.End()
	noopCtx := trace.ContextWithSpan(context.Background(), span)

	if ann := controller.TraceIDAnnotationsForTest(noopCtx); ann != nil {
		t.Fatalf("trace-id annotations = %v with tracing disabled; want nil (no fake id)", ann)
	}

	// A bare context with no span at all is likewise tracing-off: no annotation.
	if ann := controller.TraceIDAnnotationsForTest(context.Background()); ann != nil {
		t.Fatalf("trace-id annotations = %v for a span-less context; want nil", ann)
	}
}
