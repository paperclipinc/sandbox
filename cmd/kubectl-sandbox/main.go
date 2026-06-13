// Command kubectl-sandbox is a kubectl plugin that lists mitos.run sandbox
// objects. Installed as "kubectl-sandbox" on PATH, it is invoked as
// "kubectl sandbox <subcommand>".
//
// Subcommands:
//
//	ls    list SandboxClaims (NAME, POOL, PHASE, NODE, ENDPOINT, AGE)
//	ps    list SandboxForks, or one claim's forks if a name is given
//	tree  render the fork/lineage DAG (claims -> forks -> forks)
//	top   per-sandbox CoW-aware metering (unique + shared-once memory)
//	logs  husk stub pod console for a claim, plus the guest console note
//	exec  run a command in a sandbox over the token-scoped sandbox API
//
// Flags: -n <namespace>, -A (all namespaces). The plugin reads the cluster
// connection from the standard kubeconfig resolution (KUBECONFIG, --kubeconfig,
// or in-cluster). cp/port-forward remain the documented ergonomics longtail
// (issue #29).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cli/sandboxtable"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

const usage = `kubectl sandbox: inspect and operate mitos.run sandbox objects

Usage:
  kubectl sandbox ls [-n namespace] [-A]         list SandboxClaims
  kubectl sandbox ps [name] [-n namespace] [-A]  list SandboxForks (or one claim's forks)
  kubectl sandbox tree [--pool name] [-n ns] [-A] render the fork/lineage DAG
  kubectl sandbox top [-n namespace] [-A]        per-sandbox CoW-aware metering
  kubectl sandbox logs <sandbox> [-n namespace]  husk stub pod console for a claim
  kubectl sandbox exec <sandbox> [-n ns] -- cmd  run a command in a sandbox

Flags:
  -n string      namespace (default "default")
  -A             all namespaces
  --pool string  scope tree to one pool (tree only)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub := os.Args[1]

	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	var namespace string
	var allNamespaces bool
	var pool string
	fs.StringVar(&namespace, "n", "default", "namespace")
	fs.BoolVar(&allNamespaces, "A", false, "all namespaces")
	fs.StringVar(&pool, "pool", "", "scope to one pool (tree only)")

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	switch sub {
	case "ls":
		_ = fs.Parse(os.Args[2:])
		if err := runLs(namespace, allNamespaces); err != nil {
			fail(err)
		}
	case "ps":
		_ = fs.Parse(os.Args[2:])
		var name string
		if fs.NArg() > 0 {
			name = fs.Arg(0)
		}
		if err := runPs(namespace, allNamespaces, name); err != nil {
			fail(err)
		}
	case "tree":
		_ = fs.Parse(os.Args[2:])
		if err := runTree(namespace, allNamespaces, pool); err != nil {
			fail(err)
		}
	case "top":
		_ = fs.Parse(os.Args[2:])
		if err := runTop(namespace, allNamespaces); err != nil {
			fail(err)
		}
	case "logs":
		_ = fs.Parse(os.Args[2:])
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "error: logs requires a sandbox name")
			os.Exit(2)
		}
		if err := runLogs(namespace, fs.Arg(0)); err != nil {
			fail(err)
		}
	case "exec":
		// exec splits its args at "--": everything after is the command. Parse
		// only the flags before "--" so a "-flag" in the command is not eaten.
		flagArgs, cmd := splitDoubleDash(os.Args[2:])
		_ = fs.Parse(flagArgs)
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "error: exec requires a sandbox name")
			os.Exit(2)
		}
		if len(cmd) == 0 {
			fmt.Fprintln(os.Stderr, "error: exec requires a command after --")
			os.Exit(2)
		}
		if err := runExec(namespace, fs.Arg(0), cmd); err != nil {
			fail(err)
		}
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", sub, usage)
		os.Exit(2)
	}
}

// splitDoubleDash partitions args at the first "--": the elements before it are
// flag args, the elements after are the command. With no "--" the command is
// empty and all args are flag args.
func splitDoubleDash(args []string) (flagArgs, cmd []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// newClient builds a controller-runtime client from the standard kubeconfig
// resolution with the v1alpha1 scheme registered.
func newClient() (client.Client, error) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	return c, nil
}

// listOpts returns the namespace scoping for a list call: none for all
// namespaces, otherwise the chosen namespace.
func listOpts(namespace string, allNamespaces bool) []client.ListOption {
	if allNamespaces {
		return nil
	}
	return []client.ListOption{client.InNamespace(namespace)}
}

func runLs(namespace string, allNamespaces bool) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var claims v1alpha1.SandboxClaimList
	if err := c.List(ctx, &claims, listOpts(namespace, allNamespaces)...); err != nil {
		return fmt.Errorf("list claims: %w", err)
	}
	fmt.Print(sandboxtable.FormatClaims(claims.Items, time.Now()))
	return nil
}

func runPs(namespace string, allNamespaces bool, name string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var forks v1alpha1.SandboxForkList
	if err := c.List(ctx, &forks, listOpts(namespace, allNamespaces)...); err != nil {
		return fmt.Errorf("list forks: %w", err)
	}
	items := forks.Items
	if name != "" {
		filtered := items[:0:0]
		for i := range items {
			if items[i].Spec.SourceRef.Name == name {
				filtered = append(filtered, items[i])
			}
		}
		items = filtered
	}
	fmt.Print(sandboxtable.FormatForks(items, time.Now()))
	return nil
}
