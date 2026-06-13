package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func execScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func TestExecSandboxSendsBearerTokenAndDecodes(t *testing.T) {
	const token = "secret-bearer-xyz"
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"exit_code":0,"stdout":"hi\n","stderr":""}`))
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	res, err := execSandbox(context.Background(), srv.Client(), endpoint, token, "sbx-id", "echo hi")
	if err != nil {
		t.Fatalf("execSandbox: %v", err)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization header = %q, want bearer token", gotAuth)
	}
	if gotBody["sandbox"] != "sbx-id" || gotBody["command"] != "echo hi" {
		t.Errorf("exec body = %v, want sandbox/command set", gotBody)
	}
	if res.ExitCode != 0 || res.Stdout != "hi\n" {
		t.Errorf("result = %+v, want exit 0 stdout hi", res)
	}
}

func TestExecSandboxRedactsTokenFromErrorBody(t *testing.T) {
	const token = "leaky-token-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom token=" + token))
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	_, err := execSandbox(context.Background(), srv.Client(), endpoint, token, "sbx-id", "false")
	if err == nil {
		t.Fatalf("want an error from the 500")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error should show the token redacted, got %q", err.Error())
	}
}

func TestResolveSandboxAuthReadsTokenSecret(t *testing.T) {
	scheme := execScheme(t)
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Status: v1alpha1.SandboxClaimStatus{
			Phase:     v1alpha1.SandboxReady,
			Endpoint:  "10.0.0.5:9091",
			SandboxID: "sbx-id-42",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-sandbox-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("tkn-99")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, secret).Build()

	ref, endpoint, token, err := resolveSandboxAuth(context.Background(), c, "default", "sbx")
	if err != nil {
		t.Fatalf("resolveSandboxAuth: %v", err)
	}
	if ref != "sbx-id-42" {
		t.Errorf("ref = %q, want the sandbox id", ref)
	}
	if endpoint != "10.0.0.5:9091" {
		t.Errorf("endpoint = %q", endpoint)
	}
	if token != "tkn-99" {
		t.Errorf("token = %q, want the secret token", token)
	}
}

func TestResolveSandboxAuthMissingTokenErrorsClearly(t *testing.T) {
	scheme := execScheme(t)
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Status:     v1alpha1.SandboxClaimStatus{Phase: v1alpha1.SandboxReady, Endpoint: "ep:9091"},
	}
	// No token Secret in the cluster.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()

	_, _, _, err := resolveSandboxAuth(context.Background(), c, "default", "sbx")
	if err == nil {
		t.Fatalf("want an error when the token Secret is missing")
	}
	if !strings.Contains(err.Error(), "sbx-sandbox-token") {
		t.Errorf("error should name the missing token Secret, got %q", err.Error())
	}
}

func TestResolveSandboxAuthNotReadyErrorsNotHang(t *testing.T) {
	scheme := execScheme(t)
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Status:     v1alpha1.SandboxClaimStatus{Phase: v1alpha1.SandboxPending},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()

	_, _, _, err := resolveSandboxAuth(context.Background(), c, "default", "sbx")
	if err == nil {
		t.Fatalf("a not-Ready sandbox must error, not hang")
	}
	if !strings.Contains(err.Error(), "Ready") {
		t.Errorf("error should explain the sandbox is not Ready, got %q", err.Error())
	}
}

func TestResolveSandboxAuthNotFound(t *testing.T) {
	scheme := execScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, _, _, err := resolveSandboxAuth(context.Background(), c, "default", "ghost")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing claim should error with not found, got %v", err)
	}
}
