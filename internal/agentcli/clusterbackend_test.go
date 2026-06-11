package agentcli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

func TestClusterBackendCreatePollsReady(t *testing.T) {
	// Pre-seed a Ready claim so the poll returns immediately. The backend names
	// the claim deterministically only for new objects; here we drive Create and
	// then assert it created a claim and returned its name. To exercise the
	// Ready poll we use a fake client whose Create stores the object and a status
	// that the backend can read back as Ready.
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.SandboxClaim{}).
		Build()

	be := &ClusterBackend{
		client:    c,
		namespace: "default",
		now:       time.Now,
		// A short poll so the test stays fast; the backend flips the claim to
		// Ready via the readyHook injected for the test.
		pollInterval: time.Millisecond,
		pollTimeout:  2 * time.Second,
	}
	// readyHook simulates the controller reconciling the claim to Ready: as soon
	// as the backend created the claim, mark it Ready with an endpoint.
	be.readyHook = func(ctx context.Context, name string) {
		var claim v1alpha1.SandboxClaim
		if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &claim); err != nil {
			return
		}
		claim.Status.Phase = v1alpha1.SandboxReady
		claim.Status.Endpoint = "10.0.0.5:9091"
		_ = c.Status().Update(ctx, &claim)
	}

	id, err := be.Create(context.Background(), "python-pool")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatalf("Create returned an empty id")
	}

	var claim v1alpha1.SandboxClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: id}, &claim); err != nil {
		t.Fatalf("created claim not found: %v", err)
	}
	if claim.Spec.PoolRef.Name != "python-pool" {
		t.Fatalf("claim poolRef = %q, want python-pool", claim.Spec.PoolRef.Name)
	}
}

func TestClusterBackendList(t *testing.T) {
	scheme := testScheme(t)
	created := metav1.NewTime(time.Now().Add(-90 * time.Second))
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default", CreationTimestamp: created},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "python"}},
		Status: v1alpha1.SandboxClaimStatus{
			Phase: v1alpha1.SandboxReady, Node: "node-a", Endpoint: "10.0.0.1:9091",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()

	be := &ClusterBackend{client: c, namespace: "default", now: time.Now}
	infos, err := be.List(context.Background(), "default")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List len = %d, want 1", len(infos))
	}
	got := infos[0]
	if got.Name != "sbx-1" || got.Pool != "python" || got.Phase != "Ready" || got.Node != "node-a" || got.Endpoint != "10.0.0.1:9091" {
		t.Fatalf("List info = %+v, want mapped fields", got)
	}
	if got.Age < 80*time.Second || got.Age > 200*time.Second {
		t.Fatalf("List age = %v, want ~90s", got.Age)
	}
}

func TestClusterBackendTerminate(t *testing.T) {
	scheme := testScheme(t)
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: "p"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
	be := &ClusterBackend{client: c, namespace: "default", now: time.Now}

	if err := be.Terminate(context.Background(), "sbx-1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	var got v1alpha1.SandboxClaim
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sbx-1"}, &got)
	if err == nil {
		t.Fatalf("claim still exists after Terminate")
	}
}

func TestClusterBackendForkCreatesFork(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.SandboxFork{}).
		Build()
	be := &ClusterBackend{
		client: c, namespace: "default", now: time.Now,
		pollInterval: time.Millisecond, pollTimeout: 2 * time.Second,
	}
	be.forkReadyHook = func(ctx context.Context, name string, n int) {
		var fk v1alpha1.SandboxFork
		if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &fk); err != nil {
			return
		}
		forks := make([]v1alpha1.ForkInfo, 0, n)
		for i := 0; i < n; i++ {
			forks = append(forks, v1alpha1.ForkInfo{Name: name + "-" + string(rune('a'+i)), Phase: v1alpha1.SandboxReady})
		}
		fk.Status.ReadyForks = int32(n)
		fk.Status.TotalForks = int32(n)
		fk.Status.Forks = forks
		_ = c.Status().Update(ctx, &fk)
	}

	ids, err := be.Fork(context.Background(), "sbx-1", 2)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("Fork ids = %v, want 2", ids)
	}
	var forkList v1alpha1.SandboxForkList
	if err := c.List(context.Background(), &forkList); err != nil {
		t.Fatalf("list forks: %v", err)
	}
	if len(forkList.Items) != 1 {
		t.Fatalf("want 1 SandboxFork created, got %d", len(forkList.Items))
	}
	if forkList.Items[0].Spec.SourceRef.Name != "sbx-1" {
		t.Fatalf("fork sourceRef = %q, want sbx-1", forkList.Items[0].Spec.SourceRef.Name)
	}
}

func TestClusterBackendExecSendsBearerAndRedactsToken(t *testing.T) {
	const token = "super-secret-token-value"
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		if r.URL.Path == "/v1/exec" {
			// Echo the token back in the error to prove redaction protects it.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom token=` + token + `"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	endpoint := strings.TrimPrefix(srv.URL, "http://")
	scheme := testScheme(t)
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Status:     v1alpha1.SandboxClaimStatus{Phase: v1alpha1.SandboxReady, Endpoint: endpoint},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1-sandbox-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(token), "endpoint": []byte(endpoint)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, secret).Build()
	be := &ClusterBackend{client: c, namespace: "default", now: time.Now, httpClient: srv.Client()}

	_, err := be.Exec(context.Background(), "sbx-1", "echo hi", 10)
	if err == nil {
		t.Fatalf("Exec: want an error from the 500 response")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked the token: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("error should show the token redacted, got: %q", err.Error())
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization header = %q, want bearer token", gotAuth)
	}
	if gotBody["command"] != "echo hi" || gotBody["sandbox"] != "sbx-1" {
		t.Fatalf("exec body = %v, want sandbox/command set", gotBody)
	}
}

func TestClusterBackendExecSuccess(t *testing.T) {
	const token = "tkn"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"exit_code":5,"stdout":"out","stderr":"err"}`))
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	scheme := testScheme(t)
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Status:     v1alpha1.SandboxClaimStatus{Phase: v1alpha1.SandboxReady, Endpoint: endpoint},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1-sandbox-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(token)},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim, secret).Build()
	be := &ClusterBackend{client: c, namespace: "default", now: time.Now, httpClient: srv.Client()}

	res, err := be.Exec(context.Background(), "sbx-1", "ls", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 5 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("Exec result = %+v, want {5 out err}", res)
	}
}
