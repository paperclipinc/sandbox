package controller_test

// Envtest coverage for per-sandbox bearer tokens: a Ready claim (and a
// Ready fork) must produce an owned <name>-sandbox-token Secret whose token
// round-trips against the fake forkd's real HTTP sandbox API. The fake has
// no guest agent, so "auth passed" shows up as the 404 agent-missing error,
// never as a 401.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func waitClaimReady(t *testing.T, name string) *v1alpha1.SandboxClaim {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err == nil {
			if got.Status.Phase == v1alpha1.SandboxReady {
				return &got
			}
			if got.Status.Phase == v1alpha1.SandboxFailed {
				t.Fatalf("claim failed: %+v", got.Status)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("claim %s did not become Ready within 15s", name)
	return nil
}

func waitTokenSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var s corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &s); err == nil {
			return &s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("token secret %s not created within 10s", name)
	return nil
}

// execStatus POSTs an exec request to the sandbox API at endpoint and
// returns the HTTP status plus response body.
func execStatus(t *testing.T, endpoint, sandboxID, bearer string) (int, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"sandbox": sandboxID, "command": "true"})
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/v1/exec", endpoint), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func assertHex64(t *testing.T, token string) {
	t.Helper()
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
}

func TestClaimReadyCreatesOwnedTokenSecretAndGatesHTTP(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "tok-node-1", "tok-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tok-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "tok-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "tok-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "tok-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "tok-pool"},
		},
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

	got := waitClaimReady(t, "tok-claim")
	if got.Status.Endpoint == "" {
		t.Fatal("ready claim has empty endpoint")
	}

	c := newCoreClient(t)
	secret := waitTokenSecret(t, c, "tok-claim-sandbox-token")

	token := string(secret.Data["token"])
	assertHex64(t, token)
	if ep := string(secret.Data["endpoint"]); ep != got.Status.Endpoint {
		t.Fatalf("secret endpoint = %q, want %q", ep, got.Status.Endpoint)
	}

	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "SandboxClaim" || owner.Name != "tok-claim" {
		t.Fatalf("secret controller owner = %+v, want SandboxClaim tok-claim", owner)
	}

	// Token never in status or conditions.
	for _, cond := range got.Status.Conditions {
		if cond.Message != "" && bytes.Contains([]byte(cond.Message), []byte(token)) {
			t.Fatal("token leaked into a condition message")
		}
	}

	// Round-trip against the fake forkd's real HTTP handler. Without the
	// bearer: 401. With it: auth passes; the fake has no guest agent, so
	// the proof is the 404 agent-missing error, not a 401.
	status, body := execStatus(t, got.Status.Endpoint, got.Status.SandboxID, "")
	if status != 401 {
		t.Fatalf("exec without token: status = %d, body = %s, want 401", status, body)
	}
	status, body = execStatus(t, got.Status.Endpoint, got.Status.SandboxID, "0000000000000000000000000000000000000000000000000000000000000000")
	if status != 401 {
		t.Fatalf("exec with wrong token: status = %d, body = %s, want 401", status, body)
	}
	status, body = execStatus(t, got.Status.Endpoint, got.Status.SandboxID, token)
	if status != 404 {
		t.Fatalf("exec with token: status = %d, body = %s, want 404 (auth passed, no agent)", status, body)
	}
	if !bytes.Contains([]byte(body), []byte("not found or agent not connected")) {
		t.Fatalf("want agent-missing error after auth, got: %s", body)
	}
}

func TestForkReadyCreatesOwnedTokenSecret(t *testing.T) {
	stop, err := controller.StartFakeForkdNode(testRegistry, "tokf-node-1", "tokf-tmpl")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	template := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tokf-tmpl", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Image: "python:3.12-slim"},
	}
	pool := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "tokf-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "tokf-tmpl"},
			Replicas:    1,
		},
	}
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "tokf-claim", Namespace: "default"},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: "tokf-pool"},
		},
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

	waitClaimReady(t, "tokf-claim")

	forkObj := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: "tokf-fork", Namespace: "default"},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef: v1alpha1.LocalObjectReference{Name: "tokf-claim"},
			Replicas:  1,
		},
	}
	if err := k8sClient.Create(ctx, forkObj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, forkObj) })

	var forkInfo *v1alpha1.ForkInfo
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got v1alpha1.SandboxFork
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "tokf-fork", Namespace: "default"}, &got); err == nil {
			if got.Status.ReadyForks >= 1 && len(got.Status.Forks) >= 1 {
				forkInfo = &got.Status.Forks[0]
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if forkInfo == nil {
		t.Fatal("fork did not become ready within 15s")
	}

	c := newCoreClient(t)
	secret := waitTokenSecret(t, c, forkInfo.Name+"-sandbox-token")

	token := string(secret.Data["token"])
	assertHex64(t, token)
	if ep := string(secret.Data["endpoint"]); ep != forkInfo.Endpoint {
		t.Fatalf("secret endpoint = %q, want %q", ep, forkInfo.Endpoint)
	}

	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "SandboxFork" || owner.Name != "tokf-fork" {
		t.Fatalf("secret controller owner = %+v, want SandboxFork tokf-fork", owner)
	}

	// The fork's own token gates its sandbox: 401 without, agent-missing
	// 404 with.
	status, body := execStatus(t, forkInfo.Endpoint, forkInfo.SandboxID, "")
	if status != 401 {
		t.Fatalf("fork exec without token: status = %d, body = %s, want 401", status, body)
	}
	status, body = execStatus(t, forkInfo.Endpoint, forkInfo.SandboxID, token)
	if status != 404 || !bytes.Contains([]byte(body), []byte("not found or agent not connected")) {
		t.Fatalf("fork exec with token: status = %d, body = %s, want 404 agent-missing", status, body)
	}
}
