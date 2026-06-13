package facade_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentsv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	runv1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/facade"
)

// exampleImage is the test image substituted for the upstream ${IMAGE}
// placeholder in vendored example manifests. The facade does not pull it; the
// husk pool pins the real image at pool-build time. The substitution only keeps
// the upstream manifest a valid, apply-unchanged Sandbox in the test apiserver.
const exampleImage = "registry.test/example:ci"

// vendoredExampleRoots are the vendored upstream example trees we walk for core
// agents.x-k8s.io/v1alpha1 Sandbox manifests. These are upstream artifacts
// vendored verbatim under third_party/agent-sandbox; we apply them UNCHANGED
// (modulo the ${IMAGE} placeholder) against the facade. See
// docs/facade-conformance.md.
var vendoredExampleRoots = []string{
	filepath.Join("..", "..", "third_party", "agent-sandbox", "examples"),
	filepath.Join("..", "..", "third_party", "agent-sandbox", "extensions", "examples"),
}

// coreSandboxExample is one decoded core Sandbox document found in a vendored
// example file, tagged with the source path for diagnostics.
type coreSandboxExample struct {
	sourcePath string
	sandbox    *agentsv1alpha1.Sandbox
}

// loadCoreSandboxExamples walks the vendored example trees, decodes every YAML
// document, and returns each that is a core agents.x-k8s.io/v1alpha1 Sandbox
// carrying a podTemplate (skipping kustomize strategic-merge patch fragments
// that omit the full spec). The ${IMAGE} placeholder is substituted with the
// test image so the upstream manifest applies unchanged otherwise.
func loadCoreSandboxExamples(t *testing.T) []coreSandboxExample {
	t.Helper()
	var out []coreSandboxExample
	for _, root := range vendoredExampleRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
				return nil
			}
			raw, readErr := os.ReadFile(path) //nolint:gosec // vendored test fixtures
			if readErr != nil {
				t.Fatalf("read %s: %v", path, readErr)
			}
			raw = bytes.ReplaceAll(raw, []byte("${IMAGE}"), []byte(exampleImage))

			dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
			for {
				var u unstructured.Unstructured
				if decErr := dec.Decode(&u); decErr != nil {
					break
				}
				if u.Object == nil {
					continue
				}
				if u.GetAPIVersion() != "agents.x-k8s.io/v1alpha1" || u.GetKind() != "Sandbox" {
					continue
				}
				// Skip kustomize strategic-merge patch fragments (overlays): they
				// carry a podTemplate but no containers, so they are not standalone
				// applyable Sandboxes (the upstream apply path layers them onto a
				// base via kustomize). We assert the standalone manifests; the
				// overlay patches are recorded in the matrix under their base.
				containers, found, _ := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "containers")
				if !found || len(containers) == 0 {
					continue
				}
				sb := &agentsv1alpha1.Sandbox{}
				if convErr := convertUnstructured(&u, sb); convErr != nil {
					t.Fatalf("decode core Sandbox from %s: %v", path, convErr)
				}
				out = append(out, coreSandboxExample{sourcePath: path, sandbox: sb})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("found no core agents.x-k8s.io Sandbox examples under %v", vendoredExampleRoots)
	}
	return out
}

// convertUnstructured converts an unstructured object into a typed object using
// the runtime scheme registered for the test.
func convertUnstructured(u *unstructured.Unstructured, into *agentsv1alpha1.Sandbox) error {
	return scheme.Convert(u, into, nil)
}

// TestFacadeReconcilesVendoredExamples is the apply-unchanged conformance check
// at the envtest level: for every core agents.x-k8s.io/v1alpha1 Sandbox manifest
// vendored verbatim under third_party/agent-sandbox/examples (and
// extensions/examples), the facade reconciles it WITHOUT error and creates the
// bridged husk-backed SandboxClaim. The examples exercise podTemplate fields the
// facade does not yet map (volumeClaimTemplates, serviceAccountName, ports,
// command, multiple/named containers, env.valueFrom); the facade ignores the
// unmapped fields gracefully and still bridges the claim. Those unmapped fields
// are recorded as justified exceptions in docs/facade-conformance.md (no silent
// divergence). The identity (name/namespace), the pool binding (the default
// pool here, since the examples carry no mitos.run/pool annotation), and the
// first container's env are what the facade maps and asserts.
func TestFacadeReconcilesVendoredExamples(t *testing.T) {
	examples := loadCoreSandboxExamples(t)

	for _, ex := range examples {
		ex := ex
		name := strings.TrimSuffix(filepath.Base(ex.sourcePath), filepath.Ext(ex.sourcePath))
		// Disambiguate examples that share a base file name across directories.
		name = filepath.Base(filepath.Dir(ex.sourcePath)) + "/" + name
		t.Run(name, func(t *testing.T) {
			sb := ex.sandbox.DeepCopy()
			// Apply each example into the default namespace so the test does not
			// depend on namespaces the upstream manifests reference (e.g.
			// sandbox-ns). The namespace is not a field the facade maps; the
			// bridged claim lands in the same namespace as the Sandbox.
			sb.Namespace = "default"
			sb.ResourceVersion = ""
			// Several upstream examples share a metadata.name (e.g. multiple
			// "sandbox-example"/"jupyterlab" manifests across directories). Give
			// each applied Sandbox a unique name derived from its source path so
			// the per-example bridged claims do not collide. The facade reconciles
			// by Sandbox name and the name is not a mapped behavior, so renaming
			// preserves the apply-unchanged semantics of every other field.
			sb.Name = uniqueExampleName(ex.sourcePath)

			if err := k8sClient.Create(testCtx, sb); err != nil {
				t.Fatalf("apply vendored example %s unchanged: %v", ex.sourcePath, err)
			}
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })

			var claim *runv1alpha1.SandboxClaim
			eventually(t, "facade bridges a husk-backed SandboxClaim for "+ex.sourcePath, func() bool {
				c, ok := getClaimNS(t, sb.Name, sb.Namespace)
				claim = c
				return ok
			})

			// The facade binds the example to the configured default pool (the
			// examples carry no mitos.run/pool annotation) and stamps the
			// bridge annotation onto the claim.
			if claim.Spec.PoolRef.Name != "default-pool" {
				t.Fatalf("%s: claim poolRef = %q, want default-pool", ex.sourcePath, claim.Spec.PoolRef.Name)
			}
			if claim.Annotations[facade.PoolAnnotation] != "default-pool" {
				t.Fatalf("%s: claim bridge annotation = %q, want default-pool", ex.sourcePath, claim.Annotations[facade.PoolAnnotation])
			}
			// The claim is owner-referenced to the Sandbox for GC + the watch
			// back-link.
			if !hasControllerOwner(claim, sb.Name) {
				t.Fatalf("%s: claim missing controller owner reference to the Sandbox: %+v", ex.sourcePath, claim.OwnerReferences)
			}
			// The first container's env is mirrored onto the claim (the upstream
			// env, including valueFrom refs, copies through unchanged).
			wantEnv := firstContainerEnv(sb)
			if !envEqual(claim.Spec.Env, wantEnv) {
				t.Fatalf("%s: claim env = %+v, want first-container env %+v", ex.sourcePath, claim.Spec.Env, wantEnv)
			}
		})
	}
}

// uniqueExampleName derives a DNS-1123 name unique to a vendored example source
// path (the directory plus file base), so examples that share a metadata.name
// across directories do not collide when applied into the same namespace.
func uniqueExampleName(sourcePath string) string {
	base := filepath.Base(filepath.Dir(sourcePath)) + "-" + strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return "ex-" + strings.Trim(b.String(), "-")
}

func firstContainerEnv(sb *agentsv1alpha1.Sandbox) []corev1.EnvVar {
	cs := sb.Spec.PodTemplate.Spec.Containers
	if len(cs) == 0 {
		return nil
	}
	return cs[0].Env
}

func envEqual(a, b []corev1.EnvVar) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value {
			return false
		}
		if (a[i].ValueFrom == nil) != (b[i].ValueFrom == nil) {
			return false
		}
	}
	return true
}

// getClaimNS fetches our SandboxClaim by name in a given namespace.
func getClaimNS(t *testing.T, name, ns string) (*runv1alpha1.SandboxClaim, bool) {
	t.Helper()
	var claim runv1alpha1.SandboxClaim
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &claim)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get claim %s/%s: %v", ns, name, err)
	}
	return &claim, true
}
