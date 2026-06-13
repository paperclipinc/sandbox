package agentcli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// defaultClusterName is the kind cluster name DevUp/DevDown manage when
// DevOptions.ClusterName is empty.
const defaultClusterName = "mitos-dev"

// kindConfigPath is the kind cluster config DevUp passes to `kind create
// cluster`. It is repo-relative, matching how CI references it; run mitos dev
// from the repo root.
const kindConfigPath = "hack/kind-config.yaml"

// crdsPath is the directory of CRD manifests DevUp applies before the dev
// overlay. The overlay cannot reference these out-of-tree files under the
// kustomize load restrictor, so they are applied separately. Repo-relative.
const crdsPath = "deploy/crds/"

// devOverlayPath is the kustomize overlay DevUp applies after the CRDs. It runs a
// mock-mode controller (--mock --disable-pki-bootstrap), a mock-mode forkd
// DaemonSet (--mock, no KVM device, no kvm nodeSelector, no TLS), and a default
// pool, so a kind cluster forks via the mock engine and claims reach Ready
// without KVM. Repo-relative; run mitos dev from the repo root.
const devOverlayPath = "deploy/dev/"

// devPoolName is the SandboxPool the dev overlay creates; mitos sandbox
// create --pool dev-default claims from it. Exported so docs and CI agree.
const devPoolName = "dev-default"

// DevOptions configures the local dev cluster orchestration.
type DevOptions struct {
	// ClusterName overrides the kind cluster name. Empty uses defaultClusterName.
	ClusterName string
	// SkipClusterCreate makes DevUp target an already-running cluster (the
	// current kubectl context) instead of running `kind create cluster`. CI uses
	// this to apply the dev control plane onto a cluster it stood up itself.
	SkipClusterCreate bool
}

// CommandRunner runs an external command argv. DevUp/DevDown take a runner so
// the orchestration sequence is unit-testable without a real kind or kubectl;
// cmd/mitos injects a runner that shells out.
type CommandRunner func(ctx context.Context, argv []string) error

func (o DevOptions) clusterName() string {
	if o.ClusterName != "" {
		return o.ClusterName
	}
	return defaultClusterName
}

// DevUp brings a local kind dev cluster up and installs a MOCK control plane:
//
//  1. kind create cluster (tolerating an already-existing cluster; skipped when
//     opts.SkipClusterCreate targets an existing cluster, e.g. in CI)
//  2. kubectl apply -f deploy/crds/ (the CRDs)
//  3. kubectl apply -k deploy/dev/ (mock controller + mock forkd + default pool)
//
// Each external command runs through runner so the sequence is testable. The dev
// overlay runs the controller with --mock --disable-pki-bootstrap and forkd with
// --mock and no TLS, on the plain kind node (no KVM device, no kvm nodeSelector).
// The controller discovers the mock forkd, builds the pool snapshot over insecure
// gRPC, and claims fork via the mock engine and reach Ready without KVM. DevUp
// prints progress and a clear note that local dev uses the mock engine, so real
// in-VM exec needs a KVM node and the production manifests.
func DevUp(ctx context.Context, opts DevOptions, runner CommandRunner, out io.Writer) error {
	name := opts.clusterName()

	if opts.SkipClusterCreate {
		fmt.Fprintf(out, "Targeting the existing cluster (kind cluster %q assumed up); skipping kind create.\n", name)
	} else {
		fmt.Fprintf(out, "Creating kind cluster %q...\n", name)
		if err := runner(ctx, []string{"kind", "create", "cluster", "--name", name, "--config", kindConfigPath}); err != nil {
			// A cluster that already exists is not a failure: dev up is meant to be
			// re-runnable. kind reports this on stderr; the message contains
			// "already exist".
			if !isAlreadyExists(err) {
				return fmt.Errorf("create kind cluster %q: %w", name, err)
			}
			fmt.Fprintf(out, "kind cluster %q already exists, continuing.\n", name)
		}
	}

	fmt.Fprintln(out, "Applying CRDs...")
	if err := runner(ctx, []string{"kubectl", "apply", "-f", crdsPath}); err != nil {
		return fmt.Errorf("apply CRDs %s: %w", crdsPath, err)
	}

	fmt.Fprintln(out, "Applying the dev mock control plane (mock controller, mock forkd, default pool)...")
	if err := runner(ctx, []string{"kubectl", "apply", "-k", devOverlayPath}); err != nil {
		return fmt.Errorf("apply dev overlay %s: %w", devOverlayPath, err)
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Local dev cluster is up with a mock control plane.")
	fmt.Fprintf(out, "Create a sandbox: mitos sandbox create --pool %s\n", devPoolName)
	fmt.Fprintln(out, "Note: local dev uses the mock fork engine (no KVM). Claims reconcile")
	fmt.Fprintln(out, "to Ready, but real in-VM exec needs a KVM node and the production")
	fmt.Fprintln(out, "manifests (deploy/controller/ + deploy/daemon/). See docs/cli.md.")
	return nil
}

// DevDown deletes the local kind dev cluster. Deleting a non-existent cluster is
// reported by kind but is not treated as fatal here.
func DevDown(ctx context.Context, opts DevOptions, runner CommandRunner, out io.Writer) error {
	name := opts.clusterName()
	fmt.Fprintf(out, "Deleting kind cluster %q...\n", name)
	if err := runner(ctx, []string{"kind", "delete", "cluster", "--name", name}); err != nil {
		return fmt.Errorf("delete kind cluster %q: %w", name, err)
	}
	return nil
}

// isAlreadyExists reports whether err is kind's "cluster already exists" signal.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "already exist")
}
