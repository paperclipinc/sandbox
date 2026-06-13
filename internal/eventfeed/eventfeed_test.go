package eventfeed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewRevisionCreatedEnvelope(t *testing.T) {
	at := time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC)
	id := EventID("uid-123", "created")
	e := NewRevisionCreated("ns/ws-abc", id, at, RevisionCreatedData{
		Workspace:       "ws",
		Revision:        "ws-abc",
		ContentManifest: "deadbeef",
		Lineage:         "fromClaim:claim-1",
	})

	if e.SpecVersion != "1.0" {
		t.Errorf("specversion = %q, want 1.0", e.SpecVersion)
	}
	if e.Type != TypeRevisionCreated {
		t.Errorf("type = %q, want %q", e.Type, TypeRevisionCreated)
	}
	if e.Source != "mitos.run/controller" {
		t.Errorf("source = %q, want mitos.run/controller", e.Source)
	}
	if e.Subject != "ns/ws-abc" {
		t.Errorf("subject = %q", e.Subject)
	}
	if e.ID != "uid-123/created" {
		t.Errorf("id = %q, want uid-123/created", e.ID)
	}
	if !e.Time.Equal(at) {
		t.Errorf("time = %v, want %v", e.Time, at)
	}
	if e.DataContentType != "application/json" {
		t.Errorf("datacontenttype = %q", e.DataContentType)
	}

	// The marshaled body is a valid CloudEvents structured JSON document with the
	// typed data nested under "data".
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	for _, key := range []string{"specversion", "type", "source", "subject", "id", "time", "datacontenttype", "data"} {
		if _, ok := envelope[key]; !ok {
			t.Errorf("envelope missing required attribute %q", key)
		}
	}
	var data RevisionCreatedData
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.ContentManifest != "deadbeef" || data.Workspace != "ws" {
		t.Errorf("data = %+v", data)
	}
}

func TestEventIDStableForDedupe(t *testing.T) {
	a := EventID("uid-1", "created")
	b := EventID("uid-1", "created")
	if a != b {
		t.Errorf("EventID not stable: %q != %q", a, b)
	}
	if EventID("uid-1", "Ready") == EventID("uid-1", "Failed") {
		t.Error("EventID collided across sequences")
	}
	if EventID("uid-1", "created") == EventID("uid-2", "created") {
		t.Error("EventID collided across UIDs")
	}
}

func TestNopSinkIsNoOp(t *testing.T) {
	if err := (NopSink{}).Emit(context.Background(), Event{}); err != nil {
		t.Errorf("NopSink.Emit returned %v, want nil", err)
	}
	// An empty URL builds a NopSink.
	if _, ok := NewWebhookSink("").(NopSink); !ok {
		t.Error("NewWebhookSink(\"\") did not return a NopSink")
	}
}

func TestWebhookSinkPostsBody(t *testing.T) {
	var gotBody []byte
	var gotID, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotID = r.Header.Get("Ce-Id")
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := &WebhookSink{URL: srv.URL, MaxAttempts: 3, Backoff: time.Millisecond}
	at := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	e := NewRevisionCreated("ns/rev", EventID("uid-9", "created"), at, RevisionCreatedData{Workspace: "ws", Revision: "rev"})
	if err := sink.Emit(context.Background(), e); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if gotID != "uid-9/created" {
		t.Errorf("Ce-Id header = %q, want uid-9/created", gotID)
	}
	if gotContentType != "application/cloudevents+json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	var sent Event
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("server received non-JSON body: %v", err)
	}
	if sent.Type != TypeRevisionCreated || sent.ID != "uid-9/created" {
		t.Errorf("server received wrong event: %+v", sent)
	}
}

func TestWebhookSinkRetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := &WebhookSink{URL: srv.URL, MaxAttempts: 3, Backoff: time.Millisecond}
	if err := sink.Emit(context.Background(), Event{ID: "x"}); err != nil {
		t.Fatalf("Emit should have succeeded on the third try: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server hit %d times, want 3 (two 5xx then a 200)", got)
	}
}

func TestWebhookSinkGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	sink := &WebhookSink{URL: srv.URL, MaxAttempts: 2, Backoff: time.Millisecond}
	if err := sink.Emit(context.Background(), Event{ID: "x"}); err == nil {
		t.Fatal("Emit should have failed after exhausting attempts")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server hit %d times, want 2 (MaxAttempts)", got)
	}
}

func TestWebhookSinkDoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	sink := &WebhookSink{URL: srv.URL, MaxAttempts: 5, Backoff: time.Millisecond}
	if err := sink.Emit(context.Background(), Event{ID: "x"}); err == nil {
		t.Fatal("Emit should have failed on a 4xx")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server hit %d times, want 1 (a 4xx is permanent, no retry)", got)
	}
}
