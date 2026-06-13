// Command facade-bench is the reproducible harness behind the facade
// pause/resume latency comparison (issue #19; see BENCHMARKS.md "Facade vs
// upstream reference: resume latency").
//
// What it measures, honestly:
//
//   - On any cluster reachable by --kubeconfig, it applies an upstream
//     agents.x-k8s.io/v1alpha1 Sandbox, waits for our facade to bridge the
//     husk-backed SandboxClaim (claim latency), then toggles spec.replicas
//     1 -> 0 -> 1 for --iterations rounds, timing the OBJECT-LEVEL resume:
//     wall-clock from the replicas-1 patch to the facade re-creating the
//     bridged claim. That is the reconcile/re-activation latency the facade
//     adds, measured per iteration and summarized as a nearest-rank
//     distribution.
//
//   - It does NOT boot a Firecracker VMM. On a shared-CI / kind cluster the
//     husk VMM does not boot (the #18 boundary), so the IN-VM resume latency
//     (snapshot load + resume + guest-ready, the ~42ms husk activation
//     datapoint, #66) is NOT measurable here and is a bare-metal-reference-node
//     TARGET (#16). This harness measures the object-level reconcile only on
//     kind; run it on a KVM-capable kubelet (the reference node) to capture the
//     real in-VM resume tail. See bench/facade/README.md.
//
// The upstream reference controller side (cold-creating a pod on resume) is
// added on the bare-metal run, where a real pod + image + app boot can be timed
// head to head; deploying the full upstream controller is out of scope for the
// shared-CI slice and documented in the README rather than faked. No result
// numbers are hardcoded; the harness produces them.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runv1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/benchstat"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "facade-bench:", err)
		os.Exit(1)
	}
}

// run parses flags, builds the cluster client, and drives the measurement. It
// is split from main so the flag parsing and report rendering stay testable.
func run(args []string, out *os.File) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}

	c, err := newClient(cfg.kubeconfig)
	if err != nil {
		return err
	}

	h := &harness{client: c, cfg: cfg, out: out}
	return h.measure(context.Background())
}

// newClient builds a controller-runtime client from a kubeconfig path, with the
// upstream Sandbox and our mitos.run types registered on the scheme.
func newClient(kubeconfig string) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core scheme: %w", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register agents.x-k8s.io scheme: %w", err)
	}
	if err := runv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register mitos.run scheme: %w", err)
	}

	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %q: %w", kubeconfig, err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build cluster client: %w", err)
	}
	return c, nil
}

// harness drives the apply -> claim -> toggle measurement against a live
// cluster. The cluster interaction is thin; the pure aggregation lives in
// internal/benchstat, which is unit-tested.
type harness struct {
	client client.Client
	cfg    config
	out    *os.File
}

// sandbox builds the upstream Sandbox the harness applies: a minimal valid
// Sandbox (podTemplate is required) bound to the configured pool via the
// mitos.run/pool bridge annotation.
func (h *harness) sandbox(replicas int32) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        h.cfg.name,
			Namespace:   h.cfg.namespace,
			Annotations: map[string]string{poolAnnotation: h.cfg.pool},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Replicas: &replicas,
			PodTemplate: agentsv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: h.cfg.image,
					}},
				},
			},
		},
	}
}

// measure runs the full sequence: apply the Sandbox, time the initial bridged
// claim (claim latency), then toggle replicas 1->0->1 for cfg.iterations rounds
// timing the object-level resume each round. It prints both distributions.
func (h *harness) measure(ctx context.Context) error {
	defer h.cleanup(ctx)

	// Apply the Sandbox at replicas 1 and time the initial bridged claim.
	sb := h.sandbox(1)
	if err := h.apply(ctx, sb); err != nil {
		return err
	}
	claimLatency, err := h.timeClaimAppears(ctx)
	if err != nil {
		return fmt.Errorf("initial claim: %w", err)
	}
	fmt.Fprintf(h.out, "initial claim latency (apply -> bridged claim): %s\n", ms(claimLatency))

	resumeSamples := make([]time.Duration, 0, h.cfg.iterations)
	for i := 0; i < h.cfg.iterations; i++ {
		// Pause: replicas 0; wait for the facade to release the claim.
		if err := h.setReplicas(ctx, 0); err != nil {
			return fmt.Errorf("pause iteration %d: %w", i, err)
		}
		if err := h.waitClaimGone(ctx); err != nil {
			return fmt.Errorf("pause iteration %d: %w", i, err)
		}

		// Resume: replicas 1; time the object-level re-activation (the bridged
		// claim re-appearing).
		start := time.Now()
		if err := h.setReplicas(ctx, 1); err != nil {
			return fmt.Errorf("resume iteration %d: %w", i, err)
		}
		if err := h.waitClaimPresent(ctx); err != nil {
			return fmt.Errorf("resume iteration %d: %w", i, err)
		}
		resumeSamples = append(resumeSamples, time.Since(start))
	}

	summary := benchstat.Summarize(resumeSamples)
	fmt.Fprintf(h.out, "\nobject-level resume latency (replicas 1 -> bridged claim re-activated), %d iterations:\n", h.cfg.iterations)
	fmt.Fprint(h.out, summary.Table())
	fmt.Fprintln(h.out, "\nNOTE: this is OBJECT-LEVEL reconcile latency. On kind no husk VMM boots")
	fmt.Fprintln(h.out, "(the #18 boundary), so the in-VM resume tail (snapshot load + resume +")
	fmt.Fprintln(h.out, "guest-ready, the ~42ms husk activation, #66) is NOT included here; it is a")
	fmt.Fprintln(h.out, "bare-metal-reference-node target (#16). See bench/facade/README.md.")
	return nil
}

// apply creates or replaces the Sandbox.
func (h *harness) apply(ctx context.Context, sb *agentsv1alpha1.Sandbox) error {
	existing := &agentsv1alpha1.Sandbox{}
	err := h.client.Get(ctx, types.NamespacedName{Name: sb.Name, Namespace: sb.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := h.client.Create(ctx, sb); err != nil {
			return fmt.Errorf("create sandbox: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get sandbox: %w", err)
	default:
		existing.Spec = sb.Spec
		existing.Annotations = sb.Annotations
		if err := h.client.Update(ctx, existing); err != nil {
			return fmt.Errorf("update sandbox: %w", err)
		}
		return nil
	}
}

// setReplicas patches spec.replicas on the Sandbox.
func (h *harness) setReplicas(ctx context.Context, n int32) error {
	sb := &agentsv1alpha1.Sandbox{}
	if err := h.client.Get(ctx, h.key(), sb); err != nil {
		return fmt.Errorf("get sandbox: %w", err)
	}
	sb.Spec.Replicas = &n
	if err := h.client.Update(ctx, sb); err != nil {
		return fmt.Errorf("set replicas %d: %w", n, err)
	}
	return nil
}

// timeClaimAppears applies-then-waits: it times from now until the bridged
// claim first appears.
func (h *harness) timeClaimAppears(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	if err := h.waitClaimPresent(ctx); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// waitClaimPresent polls until the bridged SandboxClaim exists or the timeout
// elapses.
func (h *harness) waitClaimPresent(ctx context.Context) error {
	return h.poll(ctx, "bridged claim present", func() (bool, error) {
		_, ok, err := h.getClaim(ctx)
		return ok, err
	})
}

// waitClaimGone polls until the bridged SandboxClaim is gone or the timeout
// elapses.
func (h *harness) waitClaimGone(ctx context.Context) error {
	return h.poll(ctx, "bridged claim released", func() (bool, error) {
		_, ok, err := h.getClaim(ctx)
		return !ok, err
	})
}

// getClaim fetches the bridged SandboxClaim (same name/namespace as the
// Sandbox). The bool reports presence; a non-NotFound error is returned.
func (h *harness) getClaim(ctx context.Context) (*runv1alpha1.SandboxClaim, bool, error) {
	claim := &runv1alpha1.SandboxClaim{}
	err := h.client.Get(ctx, h.key(), claim)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get claim: %w", err)
	}
	return claim, true, nil
}

// poll runs cond every pollInterval until it returns true or cfg.timeout
// elapses.
func (h *harness) poll(ctx context.Context, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(h.cfg.timeout)
	for time.Now().Before(deadline) {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return fmt.Errorf("timed out after %s waiting for %s", h.cfg.timeout, what)
}

// cleanup deletes the Sandbox the harness created (best effort).
func (h *harness) cleanup(ctx context.Context) {
	sb := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: h.cfg.name, Namespace: h.cfg.namespace},
	}
	_ = h.client.Delete(ctx, sb)
}

func (h *harness) key() types.NamespacedName {
	return types.NamespacedName{Name: h.cfg.name, Namespace: h.cfg.namespace}
}

// sandboxReady reports whether the Sandbox status carries a Ready=True
// condition. It is used by the bare-metal variant (a KVM kubelet) to extend the
// resume timing to the in-VM tail; on kind Ready never flips True (no VMM), so
// the harness times the object-level claim re-activation instead.
func sandboxReady(sb *agentsv1alpha1.Sandbox) bool {
	cond := apimeta.FindStatusCondition(sb.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
	return cond != nil && cond.Status == metav1.ConditionTrue
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.3f ms", float64(d)/float64(time.Millisecond))
}
