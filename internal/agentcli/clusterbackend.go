package agentcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	v1alpha1 "github.com/paperclipinc/sandbox/api/v1alpha1"
	"github.com/paperclipinc/sandbox/internal/mcp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tokenSecretSuffix is appended to a claim or fork name to form the name of the
// Secret holding its sandbox API bearer token. It mirrors the controller's
// constant (internal/controller/token_secret.go).
const tokenSecretSuffix = "-sandbox-token"

// Scheme is the runtime scheme with the agentrun v1alpha1 and core types
// registered, for building a controller-runtime client against a real cluster.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

// ClusterBackend implements Backend over a Kubernetes cluster: it creates
// SandboxClaims and SandboxForks, reads the per-sandbox token Secret, and drives
// exec and file IO over the claim's HTTP endpoint with the bearer token. The
// token value is read into memory only for the duration of a request and is
// never logged; the underlying mcp.HTTPBackend redacts any echo of it from error
// strings.
type ClusterBackend struct {
	client     client.Client
	namespace  string
	httpClient *http.Client
	now        func() time.Time

	pollInterval time.Duration
	pollTimeout  time.Duration

	// readyHook / forkReadyHook are test seams: when set, they are invoked once
	// right after the claim or fork is created, simulating the controller
	// reconciling it to Ready. In production they are nil and the poll observes
	// the real controller.
	readyHook     func(ctx context.Context, name string)
	forkReadyHook func(ctx context.Context, name string, n int)
}

// NewClusterBackend builds a ClusterBackend against the cluster reachable by c,
// scoped to namespace. A nil httpClient uses http.DefaultClient.
func NewClusterBackend(c client.Client, namespace string, httpClient *http.Client) *ClusterBackend {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if namespace == "" {
		namespace = "default"
	}
	return &ClusterBackend{
		client:       c,
		namespace:    namespace,
		httpClient:   httpClient,
		now:          time.Now,
		pollInterval: 500 * time.Millisecond,
		pollTimeout:  60 * time.Second,
	}
}

// randName returns a short random suffix so generated claim and fork names do
// not collide across concurrent callers.
func randName(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// Create creates a SandboxClaim referencing pool, waits for it to reach the
// Ready phase (bounded by pollTimeout), and returns the claim name as the
// sandbox id.
func (b *ClusterBackend) Create(ctx context.Context, pool string) (string, error) {
	if pool == "" {
		return "", fmt.Errorf("create: a pool is required")
	}
	name := randName("sbx")
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace},
		Spec:       v1alpha1.SandboxClaimSpec{PoolRef: v1alpha1.LocalObjectReference{Name: pool}},
	}
	if err := b.client.Create(ctx, claim); err != nil {
		return "", fmt.Errorf("create claim: %w", err)
	}
	if b.readyHook != nil {
		b.readyHook(ctx, name)
	}
	if err := b.waitClaimReady(ctx, name); err != nil {
		return "", err
	}
	return name, nil
}

// waitClaimReady polls the claim until its phase is Ready (success), Failed
// (error), or the timeout elapses.
func (b *ClusterBackend) waitClaimReady(ctx context.Context, name string) error {
	deadline := b.now().Add(b.pollTimeout)
	for {
		var claim v1alpha1.SandboxClaim
		if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &claim); err != nil {
			return fmt.Errorf("get claim %s: %w", name, err)
		}
		switch claim.Status.Phase {
		case v1alpha1.SandboxReady:
			if claim.Status.Endpoint != "" {
				return nil
			}
		case v1alpha1.SandboxFailed:
			return fmt.Errorf("sandbox %s failed", name)
		}
		if b.now().After(deadline) {
			return fmt.Errorf("sandbox %s not ready after %s", name, b.pollTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.pollInterval):
		}
	}
}

// sandboxHTTP builds an mcp.HTTPBackend for the named claim by reading its
// endpoint and token Secret. The token is held only for the lifetime of the
// returned backend's request; the redaction in mcp.HTTPBackend keeps it out of
// any error string.
func (b *ClusterBackend) sandboxHTTP(ctx context.Context, name string) (*mcp.HTTPBackend, error) {
	var claim v1alpha1.SandboxClaim
	if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &claim); err != nil {
		return nil, fmt.Errorf("get claim %s: %w", name, err)
	}
	endpoint := claim.Status.Endpoint

	var secret corev1.Secret
	token := ""
	if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name + tokenSecretSuffix}, &secret); err == nil {
		token = string(secret.Data["token"])
		if endpoint == "" {
			endpoint = string(secret.Data["endpoint"])
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read token secret for %s: %w", name, err)
	}

	if endpoint == "" {
		return nil, fmt.Errorf("sandbox %s has no endpoint yet", name)
	}
	return mcp.NewHTTPBackend("http://"+endpoint, token, b.httpClient), nil
}

// Exec runs command in the sandbox over its HTTP endpoint with the bearer token.
func (b *ClusterBackend) Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error) {
	hb, err := b.sandboxHTTP(ctx, sandboxID)
	if err != nil {
		return ExecResult{}, err
	}
	res, err := hb.Exec(ctx, sandboxID, command, timeoutSec)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

// ReadFile reads path from the sandbox over its HTTP endpoint.
func (b *ClusterBackend) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	hb, err := b.sandboxHTTP(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	return hb.ReadFile(ctx, sandboxID, path)
}

// WriteFile writes content to path in the sandbox over its HTTP endpoint.
func (b *ClusterBackend) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	hb, err := b.sandboxHTTP(ctx, sandboxID)
	if err != nil {
		return err
	}
	return hb.WriteFile(ctx, sandboxID, path, content)
}

// Fork creates a SandboxFork sourced at sandboxID with n replicas, waits for the
// forks to be Ready (bounded), and returns the fork sandbox names.
func (b *ClusterBackend) Fork(ctx context.Context, sandboxID string, n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	name := randName(sandboxID + "-fork")
	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace},
		Spec: v1alpha1.SandboxForkSpec{
			SourceRef: v1alpha1.LocalObjectReference{Name: sandboxID},
			Replicas:  int32(n),
		},
	}
	if err := b.client.Create(ctx, fork); err != nil {
		return nil, fmt.Errorf("create fork: %w", err)
	}
	if b.forkReadyHook != nil {
		b.forkReadyHook(ctx, name, n)
	}
	return b.waitForksReady(ctx, name, n)
}

// waitForksReady polls the SandboxFork until at least n forks are Ready, then
// returns their names.
func (b *ClusterBackend) waitForksReady(ctx context.Context, name string, n int) ([]string, error) {
	deadline := b.now().Add(b.pollTimeout)
	for {
		var fork v1alpha1.SandboxFork
		if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &fork); err != nil {
			return nil, fmt.Errorf("get fork %s: %w", name, err)
		}
		ready := make([]string, 0, n)
		for i := range fork.Status.Forks {
			fi := &fork.Status.Forks[i]
			if fi.Phase == v1alpha1.SandboxReady {
				ready = append(ready, fi.Name)
			}
		}
		if len(ready) >= n {
			return ready[:n], nil
		}
		if b.now().After(deadline) {
			return nil, fmt.Errorf("fork %s not ready after %s", name, b.pollTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.pollInterval):
		}
	}
}

// Terminate deletes the SandboxClaim, which the controller reaps.
func (b *ClusterBackend) Terminate(ctx context.Context, sandboxID string) error {
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxID, Namespace: b.namespace},
	}
	if err := b.client.Delete(ctx, claim); err != nil {
		return fmt.Errorf("delete claim %s: %w", sandboxID, err)
	}
	return nil
}

// List returns the SandboxClaims in namespace mapped to SandboxInfo. An empty
// namespace lists across all namespaces.
func (b *ClusterBackend) List(ctx context.Context, namespace string) ([]SandboxInfo, error) {
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	var claims v1alpha1.SandboxClaimList
	if err := b.client.List(ctx, &claims, opts...); err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	now := b.now()
	infos := make([]SandboxInfo, 0, len(claims.Items))
	for i := range claims.Items {
		c := &claims.Items[i]
		infos = append(infos, SandboxInfo{
			Name:     c.Name,
			Pool:     c.Spec.PoolRef.Name,
			Phase:    string(c.Status.Phase),
			Node:     c.Status.Node,
			Endpoint: c.Status.Endpoint,
			Age:      now.Sub(c.CreationTimestamp.Time),
		})
	}
	return infos, nil
}
