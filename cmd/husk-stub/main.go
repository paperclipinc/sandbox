// Command husk-stub is the single-VM husk process: it brings up a DORMANT
// Firecracker VMM at start, then listens on a control Unix socket and ACTIVATES
// the VM in place by loading a snapshot when an activate request arrives. One
// husk-stub process owns exactly one VM.
//
// A husk pod is LONG-LIVED. After a successful activate the process KEEPS the
// VM alive and serving the sandbox; it tears the VM down ONLY on a terminate
// signal (SIGINT/SIGTERM) or context cancel. It does not exit (and does not
// kill the VM) just because an activate completed.
//
// The activate path drives a VMM and FAILS CLOSED: a snapshot-load or
// guest-readiness failure is reported as an error result and the VM is left
// unusable rather than reported as live. All lifecycle logging goes to stderr
// and never includes secrets.
//
// With --activate the binary instead acts as a CONTROL CLIENT: it connects to
// an already-serving stub's --control-socket, sends one ActivateRequest for
// --snapshot-dir, prints the ActivateResult as JSON on stdout, and exits 0 only
// when the result is OK. This is the in-CI driver for the activation-latency
// proof; it spawns no VMM of its own.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/husk"
)

// kvFlag collects repeatable KEY=VALUE flags into a map. It is used for --env
// and --secret. The String method NEVER renders the values: a --secret flag's
// values must not leak via usage/error output, so String reports counts only.
type kvFlag struct {
	pairs map[string]string
}

func (f *kvFlag) String() string {
	// Keys and values are intentionally omitted: a secret value must never be
	// printed. Report only how many pairs were collected.
	return fmt.Sprintf("(%d pairs)", len(f.pairs))
}

func (f *kvFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		// The error mentions only that the form is wrong, never the value, so a
		// malformed --secret does not echo its payload.
		return fmt.Errorf("expected KEY=VALUE")
	}
	if f.pairs == nil {
		f.pairs = make(map[string]string)
	}
	f.pairs[k] = val
	return nil
}

// orNil returns the collected map, or nil when empty, so an absent flag threads
// through as a nil map rather than an empty one.
func (f *kvFlag) orNil() map[string]string {
	if len(f.pairs) == 0 {
		return nil
	}
	return f.pairs
}

// loadSecretFile reads KEY=VALUE lines (one per line, blank and #-comment lines
// skipped) into m, creating it if nil. Secret values are never logged: parse
// errors mention the line number only, never the line content.
func loadSecretFile(path string, m map[string]string) (map[string]string, error) {
	file, err := os.Open(path) //nolint:gosec // operator-supplied secret file path
	if err != nil {
		return m, fmt.Errorf("open secret file: %w", err)
	}
	defer file.Close()

	if m == nil {
		m = make(map[string]string)
	}
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		k, v, ok := strings.Cut(text, "=")
		if !ok || k == "" {
			return m, fmt.Errorf("secret file line %d: expected KEY=VALUE", line)
		}
		m[k] = v
	}
	if err := scanner.Err(); err != nil {
		return m, fmt.Errorf("read secret file: %w", err)
	}
	return m, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "husk-stub: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		firecrackerBin = flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
		kernel         = flag.String("kernel", "", "path to the guest kernel image")
		workdir        = flag.String("workdir", "", "per-VM working directory (firecracker cmd.Dir; vsock UDS is bound relative to it)")
		controlSocket  = flag.String("control-socket", "", "path to the control Unix socket to listen on for activate requests")
		vcpus          = flag.Int("vcpus", 1, "guest vCPU count")
		memMiB         = flag.Int("mem-mib", 512, "guest memory in MiB")
		activate       = flag.Bool("activate", false, "act as a control CLIENT: connect to --control-socket, send one activate request for --snapshot-dir, print the result, and exit (spawns no VMM)")
		snapshotDir    = flag.String("snapshot-dir", "", "activate client mode: the template snapshot directory (expects snapshot/{mem,vmstate} layout) to activate")
		secretFile     = flag.String("secret-file", "", "activate client mode: path to a KEY=VALUE secret file (one per line) delivered to the guest; values are never logged")
	)
	var envFlag, secretFlag kvFlag
	flag.Var(&envFlag, "env", "activate client mode: repeatable KEY=VALUE guest env var")
	flag.Var(&secretFlag, "secret", "activate client mode: repeatable KEY=VALUE guest secret; values are never logged")
	flag.Parse()

	if *activate {
		secrets := secretFlag.orNil()
		if *secretFile != "" {
			var err error
			secrets, err = loadSecretFile(*secretFile, secrets)
			if err != nil {
				return err
			}
		}
		return runActivateClient(*controlSocket, *snapshotDir, envFlag.orNil(), secrets)
	}

	if *workdir == "" {
		return fmt.Errorf("--workdir is required")
	}
	if *controlSocket == "" {
		return fmt.Errorf("--control-socket is required")
	}

	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}

	cfg := firecracker.VMConfig{
		ID:             "husk",
		FirecrackerBin: *firecrackerBin,
		WorkDir:        *workdir,
		KernelPath:     *kernel,
		SocketPath:     filepath.Join(*workdir, "firecracker.sock"),
		VcpuCount:      *vcpus,
		MemSizeMib:     *memMiB,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stub := husk.New(cfg, husk.Options{})

	fmt.Fprintln(os.Stderr, "husk-stub: preparing dormant VMM")
	if err := stub.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare dormant VMM: %w", err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: dormant, state=%s\n", stub.State())

	// A husk pod is LONG-LIVED: it holds its active VM until the pod is
	// terminated. Tear the VMM down (reaping the firecracker process) ONLY on
	// shutdown, i.e. when Serve returns after a signal (SIGINT/SIGTERM) or ctx
	// cancel. We do NOT close right after a successful activate; the activated
	// VM must outlive activate so it can serve the sandbox.
	defer func() {
		if err := stub.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "husk-stub: close: %v\n", err)
		}
	}()

	// Fresh control socket; a stale file from a prior run would block bind.
	_ = os.Remove(*controlSocket)
	ln, err := net.Listen("unix", *controlSocket)
	if err != nil {
		return fmt.Errorf("listen on control socket %s: %w", *controlSocket, err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: serving control socket %s\n", *controlSocket)

	// Serve blocks: it handles the activate and then keeps holding the active
	// VM, returning only on a signal (SIGINT/SIGTERM) or ctx cancel. The
	// deferred Close above then kills the VM. The VM is alive and usable for
	// the whole serving lifetime.
	if err := stub.Serve(ctx, ln); err != nil {
		return fmt.Errorf("serve control socket: %w", err)
	}
	fmt.Fprintf(os.Stderr, "husk-stub: shutting down, state=%s\n", stub.State())
	return nil
}

// runActivateClient connects to an already-serving stub's control socket, sends
// one ActivateRequest for snapshotDir, prints the ActivateResult as JSON on
// stdout, and returns an error (non-zero exit) when the result is not OK. It
// owns no VMM; it only drives the control protocol so CI can measure the
// activation latency and gate on a successful in-place activation. The
// snapshotDir carries no secrets, so it is safe to echo in the result.
//
// env and secrets are delivered to the guest by the stub after the restore
// handshake. Their VALUES are never logged here: only the result (OK, latency,
// vsock path, and any error, none of which carries a secret) is printed.
func runActivateClient(controlSocket, snapshotDir string, env, secrets map[string]string) error {
	if controlSocket == "" {
		return fmt.Errorf("--activate requires --control-socket")
	}
	if snapshotDir == "" {
		return fmt.Errorf("--activate requires --snapshot-dir")
	}

	conn, err := net.Dial("unix", controlSocket)
	if err != nil {
		return fmt.Errorf("dial control socket %s: %w", controlSocket, err)
	}
	defer conn.Close()

	if err := husk.WriteRequest(conn, husk.ActivateRequest{
		SnapshotDir: snapshotDir,
		Env:         env,
		Secrets:     secrets,
	}); err != nil {
		return fmt.Errorf("send activate request: %w", err)
	}

	res, err := husk.ReadResult(conn)
	if err != nil {
		return fmt.Errorf("read activate result: %w", err)
	}

	// Emit the full result as JSON so CI can parse LatencyMs and VsockPath.
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("encode activate result: %w", err)
	}

	if !res.OK {
		return fmt.Errorf("activate failed: %s", res.Error)
	}
	return nil
}
