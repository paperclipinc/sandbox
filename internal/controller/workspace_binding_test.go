package controller_test

// Envtest coverage for binding a SandboxClaim to a Workspace (W4 slice 2).
//
// The suite's raw claim reconciler routes the workspace hydrate/dehydrate seams
// through per-test swappable fakes (setWSTransfer). These tests assert:
//   - a claim with workspaceRef + a workspace-with-head hydrates the head
//     manifest into the sandbox on activate;
//   - terminate dehydrates and creates a new committed WorkspaceRevision with
//     fromClaim lineage, and the workspace head advances to it;
//   - the dehydrate is passed the secret exclude list;
//   - a second claim on a busy workspace pends with reason WorkspaceBusy;
//   - a claim without workspaceRef is unaffected (no hydrate/dehydrate).

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/workspace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// wsRecorder records what the hydrate/dehydrate fakes were asked to do, so a
// test can assert the seam was invoked with the right manifest and excludes.
type wsRecorder struct {
	mu              sync.Mutex
	hydratedDigest  cas.Digest
	hydrateCalls    int
	dehydrateCalls  int
	excludesPassed  []string
	capturesPassed  []string
	dehydrateDigest cas.Digest
}

func (r *wsRecorder) install(t *testing.T, dehydrateDigest cas.Digest) {
	r.dehydrateDigest = dehydrateDigest
	setWSTransfer(
		func(_ context.Context, _ *v1alpha1.SandboxClaim, manifest cas.Digest) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.hydrateCalls++
			r.hydratedDigest = manifest
			return nil
		},
		func(_ context.Context, _ *v1alpha1.SandboxClaim, excludePaths, capturePaths []string) (cas.Digest, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dehydrateCalls++
			r.excludesPassed = append([]string(nil), excludePaths...)
			r.capturesPassed = append([]string(nil), capturePaths...)
			return r.dehydrateDigest, nil
		},
	)
	t.Cleanup(func() { setWSTransfer(nil, nil) })
}

func (r *wsRecorder) snapshot() (hydrateCalls int, hydrated cas.Digest, dehydrateCalls int, excludes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hydrateCalls, r.hydratedDigest, r.dehydrateCalls, append([]string(nil), r.excludesPassed...)
}

func (r *wsRecorder) captures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.capturesPassed...)
}

// makeBoundClaim creates a template, pool, and claim bound to wsName.
func makeBoundClaim(t *testing.T, prefix, wsName string, spec v1alpha1.SandboxClaimSpec) *v1alpha1.SandboxClaim {
	t.Helper()
	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: prefix + "-tmpl"},
			Replicas:    1,
		},
	}
	spec.PoolRef = v1alpha1.LocalObjectReference{Name: prefix + "-pool"}
	if wsName != "" {
		spec.WorkspaceRef = &v1alpha1.LocalObjectReference{Name: wsName}
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-claim", Namespace: "default"},
		Spec:       spec,
	}
	for _, obj := range []client.Object{template, pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
		_ = k8sClient.Delete(ctx, template)
	})
	return claim
}

// waitBoundPhase waits until the named claim reaches the given phase, reusing the
// shared predicate-based waitClaimPhase helper.
func waitBoundPhase(t *testing.T, name string, phase v1alpha1.SandboxPhase) *v1alpha1.SandboxClaim {
	t.Helper()
	return waitClaimPhase(t, name, func(c *v1alpha1.SandboxClaim) bool {
		return c.Status.Phase == phase
	})
}

func claimReadyReason(t *testing.T, name string) (phase v1alpha1.SandboxPhase, reason string) {
	t.Helper()
	var got v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" {
			return got.Status.Phase, c.Reason
		}
	}
	return got.Status.Phase, ""
}

func TestClaimWorkspaceHydrateOnActivate(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0xaa)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-hydrate-node", "wsh-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	headManifest := testManifest(0xbe)
	makeWorkspace(t, "ws-bind-hydrate", v1alpha1.WorkspaceRetention{})
	makeRevision(t, "ws-bind-hydrate-r1", "ws-bind-hydrate", headManifest, nil, nil)
	// Wait for the workspace head to converge to the committed revision.
	waitWorkspace(t, "ws-bind-hydrate", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head == "ws-bind-hydrate-r1"
	}, "head committed")

	makeBoundClaim(t, "wsh", "ws-bind-hydrate", v1alpha1.SandboxClaimSpec{NodeName: "ws-hydrate-node"})
	waitBoundPhase(t, "wsh-claim", v1alpha1.SandboxReady)

	// The hydrate seam must have been invoked with the head's manifest.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		calls, hydrated, _, _ := rec.snapshot()
		if calls >= 1 {
			if string(hydrated) != headManifest {
				t.Fatalf("hydrate invoked with manifest %q, want head %q", hydrated, headManifest)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("hydrate seam was not invoked for the bound claim within 10s")
}

func TestClaimWorkspaceDehydrateOnTerminate(t *testing.T) {
	rec := &wsRecorder{}
	revDigest := cas.Digest(testManifest(0xcd))
	rec.install(t, revDigest)

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-dehydrate-node", "wsd-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-bind-dehydrate", v1alpha1.WorkspaceRetention{})
	// No head yet: the claim starts empty, then dehydrate creates the first
	// committed revision on terminate.

	claim := makeBoundClaim(t, "wsd", "ws-bind-dehydrate", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-dehydrate-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "wsd-claim", v1alpha1.SandboxReady)

	// Lifetime expiry terminates the claim, which dehydrates first.
	waitBoundPhase(t, "wsd-claim", v1alpha1.SandboxTerminated)

	// A dehydrate must have run with the secret exclude list.
	deadline := time.Now().Add(10 * time.Second)
	var excludes []string
	for time.Now().Before(deadline) {
		_, _, dcalls, ex := rec.snapshot()
		if dcalls >= 1 {
			excludes = ex
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if len(excludes) == 0 {
		t.Fatal("dehydrate seam was not invoked with an exclude list")
	}
	if !containsExclude(excludes, "/workspace/.netrc") {
		t.Fatalf("dehydrate exclude list %v missing the secret paths", excludes)
	}

	// A new committed WorkspaceRevision with fromClaim lineage must exist and the
	// workspace head must advance to it.
	ws := waitWorkspace(t, "ws-bind-dehydrate", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != "" && ws.Status.Revisions >= 1
	}, "head advanced after dehydrate")

	var head v1alpha1.WorkspaceRevision
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: ws.Status.Head}, &head); err != nil {
		t.Fatalf("get head revision: %v", err)
	}
	if head.Spec.Source.FromClaim != claim.Name {
		t.Fatalf("head revision fromClaim = %q, want %q", head.Spec.Source.FromClaim, claim.Name)
	}
	if head.Spec.ContentManifest != string(revDigest) {
		t.Fatalf("head revision contentManifest = %q, want the dehydrate digest %q", head.Spec.ContentManifest, revDigest)
	}
}

func TestClaimWorkspaceSingleWriterBusy(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0xef)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-busy-node", "wsb-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-busy", v1alpha1.WorkspaceRetention{})

	// First claim binds and goes Ready.
	makeBoundClaim(t, "wsb1", "ws-busy", v1alpha1.SandboxClaimSpec{NodeName: "ws-busy-node"})
	waitBoundPhase(t, "wsb1-claim", v1alpha1.SandboxReady)

	// Second claim on the same workspace must pend with WorkspaceBusy.
	makeBoundClaim(t, "wsb2", "ws-busy", v1alpha1.SandboxClaimSpec{NodeName: "ws-busy-node"})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		phase, reason := claimReadyReason(t, "wsb2-claim")
		if phase == v1alpha1.SandboxPending && reason == "WorkspaceBusy" {
			return
		}
		if phase == v1alpha1.SandboxReady {
			t.Fatal("second claim on a busy workspace went Ready; single-writer not enforced")
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("second claim did not pend with WorkspaceBusy within 15s")
}

func TestClaimWithoutWorkspaceRefUnaffected(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0x11)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-none-node", "wsn-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeBoundClaim(t, "wsn", "", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-none-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
	})
	waitBoundPhase(t, "wsn-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wsn-claim", v1alpha1.SandboxTerminated)

	// Neither hydrate nor dehydrate may have been invoked for an unbound claim.
	hcalls, _, dcalls, _ := rec.snapshot()
	if hcalls != 0 || dcalls != 0 {
		t.Fatalf("unbound claim invoked the transfer seams: hydrate=%d dehydrate=%d", hcalls, dcalls)
	}
}

// TestClaimWorkspaceOutputsCapturePaths asserts that a claim whose
// spec.outputs lists a {path} narrows the dehydrate capture to that subtree:
// the dehydrate seam is invoked with the normalized capture path.
func TestClaimWorkspaceOutputsCapturePaths(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0x21)))

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-out-node", "wso-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-outputs", v1alpha1.WorkspaceRetention{})

	makeBoundClaim(t, "wso", "ws-outputs", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-out-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
		Outputs:  []v1alpha1.OutputSpec{{Path: "/workspace/dist"}},
	})
	waitBoundPhase(t, "wso-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wso-claim", v1alpha1.SandboxTerminated)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, _, dcalls, _ := rec.snapshot()
		if dcalls >= 1 {
			captures := rec.captures()
			if len(captures) != 1 || captures[0] != "dist" {
				t.Fatalf("dehydrate capture paths = %v, want [dist]", captures)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("dehydrate seam was not invoked for the outputs claim within 10s")
}

// TestClaimWorkspaceOutputsDiffRecorded asserts that a {diff: true} output makes
// the new revision record a content-hash diff summary against the parent head.
func TestClaimWorkspaceOutputsDiffRecorded(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0x33)))

	// Scripted diff: the diff seam reports one added and one modified path.
	setWSDiff(func(_ context.Context, _ *v1alpha1.SandboxClaim, _, _ cas.Digest) (workspace.Diff, error) {
		return workspace.Diff{Added: []string{"new.txt"}, Modified: []string{"main.go"}}, nil
	})
	t.Cleanup(func() { setWSDiff(nil) })

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-diff-node", "wsdf-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	makeWorkspace(t, "ws-diff", v1alpha1.WorkspaceRetention{})

	makeBoundClaim(t, "wsdf", "ws-diff", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-diff-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
		Outputs:  []v1alpha1.OutputSpec{{Diff: true}},
	})
	waitBoundPhase(t, "wsdf-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wsdf-claim", v1alpha1.SandboxTerminated)

	ws := waitWorkspace(t, "ws-diff", func(ws *v1alpha1.Workspace) bool {
		return ws.Status.Head != ""
	}, "head advanced after dehydrate")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var head v1alpha1.WorkspaceRevision
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: ws.Status.Head}, &head); err == nil {
			if head.Status.DiffSummary != nil {
				ds := head.Status.DiffSummary
				if len(ds.Added) == 1 && ds.Added[0] == "new.txt" &&
					len(ds.Modified) == 1 && ds.Modified[0] == "main.go" &&
					ds.AddedCount == 1 && ds.ModifiedCount == 1 {
					return
				}
				t.Fatalf("revision diff summary = %+v, want added [new.txt] modified [main.go]", ds)
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("revision did not record a diff summary within 10s")
}

// TestClaimWorkspaceGitOutputPushes asserts that a {git} output on a workspace
// with spec.git.paths renders the per-attempt branch, calls the rendezvous seam
// with the resolved repo files, and records the push on the new revision status.
func TestClaimWorkspaceGitOutputPushes(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0x44)))

	// The repo-paths resolver returns one file under the git path.
	setWSRepoFiles(func(_ context.Context, _ *v1alpha1.SandboxClaim, _ cas.Digest, gitPaths []string) (map[string]string, error) {
		return map[string]string{"repo/main.go": "package main\n"}, nil
	})
	t.Cleanup(func() { setWSRepoFiles(nil) })

	var (
		gitMu      sync.Mutex
		gotRemote  string
		gotBranch  string
		gotFiles   map[string]string
		pushCalled int
	)
	setWSRendezvous(func(_ context.Context, repoFiles map[string]string, remote, branch string, _ *workspace.Credentials) error {
		gitMu.Lock()
		defer gitMu.Unlock()
		pushCalled++
		gotRemote, gotBranch, gotFiles = remote, branch, repoFiles
		return nil
	})
	t.Cleanup(func() { setWSRendezvous(nil) })

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-git-node", "wsg-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	ws := makeWorkspace(t, "ws-git", v1alpha1.WorkspaceRetention{})
	// Re-Get inside RetryOnConflict so each attempt carries a fresh
	// resourceVersion: the WorkspaceReconciler updates the workspace status
	// concurrently, so a plain Update here loses an optimistic-lock race
	// intermittently.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, ws); err != nil {
			return err
		}
		ws.Spec.Git = v1alpha1.WorkspaceGit{Paths: []string{"/workspace/repo"}}
		return k8sClient.Update(ctx, ws)
	}); err != nil {
		t.Fatalf("set workspace git paths: %v", err)
	}

	makeBoundClaim(t, "wsg", "ws-git", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-git-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
		Outputs:  []v1alpha1.OutputSpec{{Git: &v1alpha1.GitOutput{Remote: "file:///srv/git/rendezvous.git", Branch: "attempt/{{.name}}"}}},
	})
	waitBoundPhase(t, "wsg-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wsg-claim", v1alpha1.SandboxTerminated)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		gitMu.Lock()
		called, remote, branch, files := pushCalled, gotRemote, gotBranch, gotFiles
		gitMu.Unlock()
		if called >= 1 {
			if remote != "file:///srv/git/rendezvous.git" {
				t.Fatalf("rendezvous remote = %q, want file:///srv/git/rendezvous.git", remote)
			}
			if branch != "attempt/wsg-claim" {
				t.Fatalf("rendezvous branch = %q, want attempt/wsg-claim", branch)
			}
			if files["repo/main.go"] != "package main\n" {
				t.Fatalf("rendezvous repo files = %v, missing repo/main.go", files)
			}
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if pushCalled == 0 {
		t.Fatal("git rendezvous seam was not invoked within 10s")
	}

	// The push must be recorded on the new revision status.
	wsHead := waitWorkspace(t, "ws-git", func(w *v1alpha1.Workspace) bool { return w.Status.Head != "" }, "head advanced")
	for time.Now().Before(deadline) {
		var head v1alpha1.WorkspaceRevision
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: wsHead.Status.Head}, &head); err == nil {
			if len(head.Status.GitPushes) == 1 &&
				head.Status.GitPushes[0].Branch == "attempt/wsg-claim" &&
				head.Status.GitPushes[0].Remote == "file:///srv/git/rendezvous.git" {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("revision did not record the git push within 10s")
}

// TestClaimWorkspaceGitCredentialsResolvedAndNeverLeak asserts that a workspace
// with spec.git.credentialsSecretRef has its token resolved from the Secret and
// passed to the rendezvous seam, and that the token VALUE never appears in any
// claim or revision condition (the secrets rule). The token only reaches the
// seam's creds argument; it is never on argv, in a log, or in a condition.
func TestClaimWorkspaceGitCredentialsResolvedAndNeverLeak(t *testing.T) {
	rec := &wsRecorder{}
	rec.install(t, cas.Digest(testManifest(0x45)))

	setWSRepoFiles(func(_ context.Context, _ *v1alpha1.SandboxClaim, _ cas.Digest, _ []string) (map[string]string, error) {
		return map[string]string{"repo/main.go": "package main\n"}, nil
	})
	t.Cleanup(func() { setWSRepoFiles(nil) })

	const token = "ghp_envtest_TOKEN_DEADBEEF" //nolint:gosec // test sentinel
	var (
		gitMu     sync.Mutex
		gotCreds  *workspace.Credentials
		pushCalls int
	)
	setWSRendezvous(func(_ context.Context, _ map[string]string, _, _ string, creds *workspace.Credentials) error {
		gitMu.Lock()
		defer gitMu.Unlock()
		pushCalls++
		gotCreds = creds
		return nil
	})
	t.Cleanup(func() { setWSRendezvous(nil) })

	// The credentials Secret holding the push token.
	if err := k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(token)},
	}); err != nil {
		t.Fatalf("create credentials secret: %v", err)
	}

	stop, err := controller.StartFakeForkdNode(testRegistry, "ws-cred-node", "wsc-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	ws := makeWorkspace(t, "ws-cred", v1alpha1.WorkspaceRetention{})
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, ws); err != nil {
			return err
		}
		ws.Spec.Git = v1alpha1.WorkspaceGit{
			Paths:                []string{"/workspace/repo"},
			CredentialsUsername:  "bot",
			CredentialsSecretRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "git-creds"}, Key: "token"},
		}
		return k8sClient.Update(ctx, ws)
	}); err != nil {
		t.Fatalf("set workspace git creds: %v", err)
	}

	makeBoundClaim(t, "wsc", "ws-cred", v1alpha1.SandboxClaimSpec{
		NodeName: "ws-cred-node",
		Timeout:  &metav1.Duration{Duration: 2 * time.Second},
		Outputs:  []v1alpha1.OutputSpec{{Git: &v1alpha1.GitOutput{Remote: "https://example.test/rendezvous.git", Branch: "attempt/{{.name}}"}}},
	})
	waitBoundPhase(t, "wsc-claim", v1alpha1.SandboxReady)
	waitBoundPhase(t, "wsc-claim", v1alpha1.SandboxTerminated)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		gitMu.Lock()
		called, creds := pushCalls, gotCreds
		gitMu.Unlock()
		if called >= 1 {
			if creds == nil {
				t.Fatal("rendezvous seam received nil credentials; the Secret was not resolved")
			}
			if creds.Username != "bot" {
				t.Fatalf("credentials username = %q, want bot", creds.Username)
			}
			if creds.Token != token {
				t.Fatalf("credentials token mismatch; the resolved token did not reach the seam")
			}
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if pushCalls == 0 {
		t.Fatal("git rendezvous seam was not invoked within 10s")
	}

	// The token VALUE must never appear in any claim condition.
	var claim v1alpha1.SandboxClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "wsc-claim"}, &claim); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	for _, c := range claim.Status.Conditions {
		if strings.Contains(c.Message, token) || strings.Contains(c.Reason, token) {
			t.Fatalf("token leaked into claim condition %s: %q", c.Type, c.Message)
		}
	}
	// And never in any revision condition or field.
	var revs v1alpha1.WorkspaceRevisionList
	if err := k8sClient.List(ctx, &revs, client.InNamespace("default")); err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	for i := range revs.Items {
		r := &revs.Items[i]
		for _, c := range r.Status.Conditions {
			if strings.Contains(c.Message, token) || strings.Contains(c.Reason, token) {
				t.Fatalf("token leaked into revision %s condition: %q", r.Name, c.Message)
			}
		}
		for _, p := range r.Status.GitPushes {
			if strings.Contains(p.Remote, token) || strings.Contains(p.Branch, token) {
				t.Fatalf("token leaked into revision %s git push record", r.Name)
			}
		}
	}
}

func containsExclude(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
