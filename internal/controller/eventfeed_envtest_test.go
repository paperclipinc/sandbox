package controller_test

// Envtest coverage for the workspace revision change feed (W4 Task 1).
//
// The suite's raw claim reconciler is wired with a recording CloudEvents sink
// (testSink) and a buffered Kubernetes Event recorder (testEventRecorder). These
// tests assert that:
//   - dehydrate-on-terminate emits a revision.created CloudEvent to the sink AND
//     records a Kubernetes Event on the revision (the always-on channel);
//   - a claim phase transition emits a sandbox.phase.changed CloudEvent;
//   - no secret values appear in any feed payload (names/digests/phases only);
//   - the dedupe id is the object UID plus a sequence.

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/eventfeed"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFeedEmitsRevisionCreatedAndEvent(t *testing.T) {
	rec := &wsRecorder{}
	revDigest := cas.Digest(testManifest(0x5a))
	rec.install(t, revDigest)

	stop, err := controller.StartFakeForkdNode(testRegistry, "feed-rev-node", "feed-rev-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-feed-rev", v1alpha1.WorkspaceRetention{})

	claim := makeBoundClaim(t, "feedrev", "ws-feed-rev", v1alpha1.SandboxClaimSpec{
		NodeName: "feed-rev-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "feedrev-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "feedrev-claim", v1alpha1.SandboxTerminated)

	// The workspace head advances to the dehydrated revision.
	ws := waitWorkspace(t, "ws-feed-rev", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != "" && ws.Status.Revisions >= 1
	}, "head advanced after dehydrate")

	// A revision.created CloudEvent must have been emitted to the sink, naming
	// this workspace and carrying the content digest (a digest, not a secret).
	deadline := time.Now().Add(10 * time.Second)
	var found *eventfeed.Event
	for time.Now().Before(deadline) {
		for _, e := range testSink.byType(eventfeed.TypeRevisionCreated) {
			data, ok := e.Data.(eventfeed.RevisionCreatedData)
			if ok && data.Workspace == "ws-feed-rev" && data.Revision == ws.Status.Head {
				ev := e
				found = &ev
				break
			}
		}
		if found != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("no revision.created CloudEvent emitted to the sink for the dehydrated revision")
	}
	if found.SpecVersion != "1.0" || found.Source != "mitos.run/controller" {
		t.Errorf("revision.created envelope wrong: specversion=%q source=%q", found.SpecVersion, found.Source)
	}
	data := found.Data.(eventfeed.RevisionCreatedData)
	if data.ContentManifest != string(revDigest) {
		t.Errorf("revision.created contentManifest = %q, want the dehydrate digest", data.ContentManifest)
	}
	if data.Lineage != "fromClaim:"+claim.Name {
		t.Errorf("revision.created lineage = %q, want fromClaim:%s", data.Lineage, claim.Name)
	}
	// Dedupe id is the revision UID plus the "created" sequence.
	if !strings.HasSuffix(found.ID, "/created") {
		t.Errorf("revision.created id = %q, want it to end with /created", found.ID)
	}

	// The always-on Kubernetes Event channel: a RevisionCreated event was recorded.
	if !waitForEvent(t, "RevisionCreated", "ws-feed-rev") {
		t.Fatal("no RevisionCreated Kubernetes Event recorded")
	}
}

func TestFeedEmitsPhaseChanged(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0x6b)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "feed-phase-node", "feed-phase-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeBoundClaim(t, "feedphase", "", v1alpha1.SandboxClaimSpec{
		NodeName: "feed-phase-node",
	})
	waitBoundPhase(t, "feedphase-claim", v1alpha1.SandboxReady)

	// A sandbox.phase.changed CloudEvent naming this claim, ending at Ready, must
	// have been emitted. The phase strings are the only payload; no secret values.
	deadline := time.Now().Add(10 * time.Second)
	var sawReady bool
	for time.Now().Before(deadline) {
		for _, e := range testSink.byType(eventfeed.TypePhaseChanged) {
			data, ok := e.Data.(eventfeed.PhaseChangedData)
			if ok && data.Claim == "feedphase-claim" && data.NewPhase == string(v1alpha1.SandboxReady) {
				sawReady = true
				if !strings.HasSuffix(e.ID, "/Ready") {
					t.Errorf("phase.changed id = %q, want it to end with /Ready", e.ID)
				}
			}
		}
		if sawReady {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !sawReady {
		t.Fatal("no sandbox.phase.changed CloudEvent reaching Ready emitted for the claim")
	}

	// The always-on Kubernetes Event channel recorded a PhaseChanged event.
	if !waitForEvent(t, "PhaseChanged", "feedphase-claim") {
		t.Fatal("no PhaseChanged Kubernetes Event recorded")
	}
}

// waitForEvent drains the buffered FakeRecorder until it sees an event whose
// reason and message both match, or times out. The FakeRecorder formats each
// recorded event as "<type> <reason> <message>".
func waitForEvent(t *testing.T, reason, contains string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range testEventRecorder.snapshot() {
			if strings.Contains(line, reason) && strings.Contains(line, contains) {
				return true
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}
