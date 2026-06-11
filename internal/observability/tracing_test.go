package observability_test

import (
	"context"
	"testing"

	"github.com/paperclipinc/sandbox/internal/observability"
	"go.opentelemetry.io/otel"
)

// TestSetupDisabledIsNoop asserts that with no endpoint, Setup installs no real
// provider: spans started through the global tracer are non-recording, so
// tracing is zero cost when disabled.
func TestSetupDisabledIsNoop(t *testing.T) {
	shutdown, err := observability.Setup(context.Background(), "test-svc", "")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	_, span := otel.Tracer("t").Start(context.Background(), "noop")
	defer span.End()
	if span.IsRecording() {
		t.Fatal("span is recording with tracing disabled; expected a no-op span")
	}
}

// TestInMemoryForTestRecordsSpans asserts the in-memory helper installs a
// recorder so spans started via the global tracer are captured, and that
// restore puts the prior provider back.
func TestInMemoryForTestRecordsSpans(t *testing.T) {
	recorder, restore := observability.InMemoryForTest()

	_, span := observability.Tracer("t").Start(context.Background(), "unit.span")
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if ended[0].Name() != "unit.span" {
		t.Fatalf("span name = %q, want unit.span", ended[0].Name())
	}

	restore()

	// After restore the recorder no longer captures new spans.
	_, span2 := observability.Tracer("t").Start(context.Background(), "after.restore")
	span2.End()
	if got := len(recorder.Ended()); got != 1 {
		t.Fatalf("recorder captured %d spans after restore, want 1", got)
	}
}
