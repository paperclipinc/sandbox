package controller_test

// Unit coverage for the trace-id correlation carried in the revision.created
// feed event (W4 observability slice). The revision.created CloudEvent gains a
// TraceID field read from the revision's mitos.run/trace-id annotation, so an
// external indexer correlates the revision event with the orchestrator trace.
// The id is an opaque correlation id, never a secret; it is empty when the
// revision carries no annotation (tracing was disabled).

import (
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/eventfeed"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func revisionCreatedData(t *testing.T, sink *recordingSink) eventfeed.RevisionCreatedData {
	t.Helper()
	evs := sink.byType(eventfeed.TypeRevisionCreated)
	if len(evs) != 1 {
		t.Fatalf("recorded %d revision.created events, want 1", len(evs))
	}
	data, ok := evs[0].Data.(eventfeed.RevisionCreatedData)
	if !ok {
		t.Fatalf("revision.created data is %T, want RevisionCreatedData", evs[0].Data)
	}
	return data
}

// TestRevisionCreatedCarriesTraceID asserts the feed event carries the trace id
// stamped on the revision (the mitos.run/trace-id annotation).
func TestRevisionCreatedCarriesTraceID(t *testing.T) {
	const traceID = "7b743a0c9f1cedb209c9e796151158aa"
	sink := &recordingSink{}
	rev := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ws-x-abcde",
			Namespace:   "default",
			Annotations: map[string]string{"mitos.run/trace-id": traceID},
		},
		Spec: v1alpha1.WorkspaceRevisionSpec{
			WorkspaceRef:    v1alpha1.LocalObjectReference{Name: "ws-x"},
			Source:          v1alpha1.RevisionSource{FromClaim: "claim-x"},
			ContentManifest: "deadbeef",
		},
	}

	controller.EmitRevisionCreatedForTest(nil, sink, rev)

	data := revisionCreatedData(t, sink)
	if data.TraceID != traceID {
		t.Fatalf("revision.created TraceID = %q, want %q", data.TraceID, traceID)
	}
}

// TestRevisionCreatedTraceIDEmptyWithoutAnnotation asserts the trace id is empty
// when the revision carries no mitos.run/trace-id annotation (tracing off).
func TestRevisionCreatedTraceIDEmptyWithoutAnnotation(t *testing.T) {
	sink := &recordingSink{}
	rev := &v1alpha1.WorkspaceRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-y-fghij", Namespace: "default"},
		Spec: v1alpha1.WorkspaceRevisionSpec{
			WorkspaceRef:    v1alpha1.LocalObjectReference{Name: "ws-y"},
			Source:          v1alpha1.RevisionSource{FromClaim: "claim-y"},
			ContentManifest: "cafef00d",
		},
	}

	controller.EmitRevisionCreatedForTest(nil, sink, rev)

	data := revisionCreatedData(t, sink)
	if data.TraceID != "" {
		t.Fatalf("revision.created TraceID = %q, want empty without the annotation", data.TraceID)
	}
}
