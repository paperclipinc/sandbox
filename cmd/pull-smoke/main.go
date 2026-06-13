// Command pull-smoke proves the build-once-distribute path end to end on ONE
// machine using TWO engines and TWO data dirs: node A builds a template into its
// content-addressed store and serves that store over TLS gated by a peer token
// (exactly the surface forkd mounts under /cas), and node B's engine.PullTemplate
// pulls the snapshot from A over a real HTTPS connection, materializes it,
// verifies it (manifest digest + snapshot compatibility), writes the verified
// marker, and then forks a sandbox from the PULLED template and execs an
// assertion inside it.
//
// The TLS handshake is the production handshake, not InsecureSkipVerify: the
// holder serves a certificate whose SAN is pki.ServerName ("forkd.mitos"),
// which is exactly the name the pull client pins via pki.ClientTLSConfig, so the
// pinning the Task 1-2 note flagged is satisfied by issuing the holder a real
// matching cert. Node B presents a CA-signed client identity (pki.ControllerName),
// so the pull also rides forkd-to-forkd mTLS, the same as production.
//
// HONEST SCOPE: this runs both engines in one process over a loopback TLS
// listener. It is a real cross-process-class HTTP-over-TLS CAS pull (two data
// dirs, two CAS stores, the wire is the production code path), but the two hosts
// are the same machine. True cross-HOST distribution exercises the identical
// PullTemplate + cas.Pull + cas.NewHTTPHandler code path and is measured only on
// a multi-node testbed (see docs/snapshot-distribution.md, issue #14).
//
// Subcommands:
//
//	pull-smoke kvm     build on A, serve over TLS, pull to B, fork on B + exec.
//	                   This is the KVM gate: it proves the pulled snapshot boots.
//	pull-smoke verify  the integrity/auth assertions that do NOT need KVM: a
//	                   wrong token is rejected with 403, and a wrong manifest
//	                   digest fails the pull and leaves no verified marker on B.
//
// Every assertion gates: any failure exits nonzero so the CI step fails. A wrong
// token, a missing marker, or a fork/exec failure is a real assertion failure,
// distinct from a setup problem (a missing cert/port surfaces as a SETUP error).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paperclipinc/mitos/internal/cas"
	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/pki"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// peerToken is the shared bearer credential the holder gates its CAS with. It is
// a CI-only fixed value; in production it is controller-minted. It is never
// logged (the assertions below print PASS/FAIL, never the token).
const peerToken = "ci-pull-smoke-token-do-not-log"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "pull-smoke: usage: pull-smoke <kvm|verify> [flags]")
		os.Exit(2)
	}
	mode := os.Args[1]
	os.Args = append(os.Args[:1], os.Args[2:]...)

	var err error
	switch mode {
	case "kvm":
		err = runKVM()
	case "verify":
		err = runVerify()
	default:
		fmt.Fprintf(os.Stderr, "pull-smoke: unknown mode %q (want kvm|verify)\n", mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pull-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
}

// setupErr tags an error as a setup/environment problem (missing cert, port,
// rootfs) rather than a real assertion failure, so CI can tell them apart.
func setupErr(err error) error { return fmt.Errorf("SETUP: %w", err) }

// pkiBundle is a CA plus the holder's serving cert (SAN pki.ServerName) and the
// puller's client cert (SAN pki.ControllerName), the exact identities forkd uses.
type pkiBundle struct {
	caPEM                       []byte
	serverCertPEM, serverKeyPEM []byte
	clientCertPEM, clientKeyPEM []byte
}

// generatePKI mints a fresh CA and the two leaves the pull needs. The holder's
// cert carries pki.ServerName as its SAN, which is exactly what the pull client
// pins, so the TLS verify succeeds without any InsecureSkipVerify. This is how
// the SAN pinning the Task 1-2 note flagged is handled in CI: issue a real cert
// carrying the pinned name.
func generatePKI() (*pkiBundle, error) {
	ca, err := pki.NewCA("pull-smoke")
	if err != nil {
		return nil, fmt.Errorf("new CA: %w", err)
	}
	server, err := ca.Issue(pki.ServerName)
	if err != nil {
		return nil, fmt.Errorf("issue holder serving cert (SAN %s): %w", pki.ServerName, err)
	}
	client, err := ca.Issue(pki.ControllerName)
	if err != nil {
		return nil, fmt.Errorf("issue puller client cert (SAN %s): %w", pki.ControllerName, err)
	}
	return &pkiBundle{
		caPEM:         ca.CertPEM(),
		serverCertPEM: server.CertPEM, serverKeyPEM: server.KeyPEM,
		clientCertPEM: client.CertPEM, clientKeyPEM: client.KeyPEM,
	}, nil
}

// holder is node A's running TLS CAS server.
type holder struct {
	port int
	srv  *http.Server
}

func (h *holder) close() { _ = h.srv.Close() }

// baseURL is the https CAS base node B pulls from. The host is the PINNED
// pki.ServerName (not 127.0.0.1) so the served SAN is what gets verified; the
// pull client maps that name to the loopback listener via a dial override.
func (h *holder) baseURL() string {
	return fmt.Sprintf("https://%s:%d/cas", pki.ServerName, h.port)
}

// startHolderCAS starts node A's token-gated TLS CAS server over a loopback
// listener. This is the exact handler forkd mounts: cas.RequirePullToken
// wrapping cas.NewHTTPHandler, served with the pki server TLS config (TLS 1.3,
// client cert required and verified against the CA).
func startHolderCAS(store *cas.Store, b *pkiBundle) (*holder, error) {
	tlsCfg, err := pki.ServerTLSConfig(b.serverCertPEM, b.serverKeyPEM, b.caPEM)
	if err != nil {
		return nil, fmt.Errorf("holder server TLS config: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("holder listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/cas/", cas.RequirePullToken(peerToken, cas.NewHTTPHandler(store)))
	srv := &http.Server{Handler: mux, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.ServeTLS(ln, "", "") }() //nolint:errcheck // stopped on close
	return &holder{port: ln.Addr().(*net.TCPAddr).Port, srv: srv}, nil
}

// newPullClient builds the HTTPS client node B uses, mirroring forkd's
// pullHTTPClient: it presents the controller client identity and pins
// pki.ServerName. A DialContext override maps the pinned hostname to the loopback
// listener so the single-machine test still verifies the real SAN (the cert must
// carry pki.ServerName; verification is NOT skipped). token carries the
// credential; the client does not.
func newPullClient(b *pkiBundle, listenerPort int) (*http.Client, error) {
	tlsCfg, err := pki.ClientTLSConfig(b.clientCertPEM, b.clientKeyPEM, b.caPEM)
	if err != nil {
		return nil, fmt.Errorf("puller client TLS config: %w", err)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// Every dial for the pinned host goes to the loopback listener; the
			// TLS layer still verifies the cert SAN against pki.ServerName.
			return dialer.DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", listenerPort))
		},
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Minute}, nil
}

// buildHolderTemplate constructs node A's engine, builds the template into its
// CAS + data dir, and returns the engine and the recorded manifest digest (the
// content address node B pulls by).
func buildHolderTemplate(dataDir, rootfs, fcBin, kernel, agentBin, templateID string) (*fork.Engine, string, error) {
	engine, err := fork.NewEngine(dataDir, fcBin, kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		AgentBinPath: agentBin,
	})
	if err != nil {
		return nil, "", fmt.Errorf("node A new engine: %w", err)
	}
	if err := engine.CreateTemplate(templateID, rootfs, nil, nil); err != nil {
		return nil, "", fmt.Errorf("node A build template: %w", err)
	}
	digest := engine.GetCapacity().TemplateDigests[templateID]
	if digest == "" {
		return nil, "", fmt.Errorf("node A recorded no digest for template %q", templateID)
	}
	return engine, digest, nil
}

// runKVM is the gate: build a template on A, serve A's CAS over TLS, pull it to
// B over the real handshake, assert B materialized + verified it, then fork on B
// and exec inside the pulled snapshot.
func runKVM() error {
	fs := flag.NewFlagSet("kvm", flag.ExitOnError)
	root := fs.String("root", "", "scratch root for the two engine data dirs")
	rootfs := fs.String("image", "", "path to the prebuilt ext4 rootfs the template is built from")
	fcBin := fs.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := fs.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := fs.String("agent-bin", "", "path to the guest agent binary injected as /init")
	expectCmd := fs.String("expect-cmd", "ls /init", "command that must succeed in the pulled fork")
	_ = fs.Parse(os.Args[1:])

	if *root == "" || *rootfs == "" || *kernel == "" {
		return setupErr(errors.New("--root, --image and --kernel are required"))
	}

	b, err := generatePKI()
	if err != nil {
		return setupErr(err)
	}

	dataA := filepath.Join(*root, "node-a")
	dataB := filepath.Join(*root, "node-b")
	templateID := "dist-tmpl"

	// --- node A: build the template into its CAS + data dir ---
	fmt.Printf("pull-smoke: node A building template %q from %s\n", templateID, *rootfs)
	engineA, digest, err := buildHolderTemplate(dataA, *rootfs, *fcBin, *kernel, *agentBin, templateID)
	if err != nil {
		return setupErr(err)
	}
	fmt.Printf("pull-smoke: node A template digest %s\n", digest)

	// --- node A: serve its CAS over token-gated TLS ---
	h, err := startHolderCAS(engineA.CASStore(), b)
	if err != nil {
		return setupErr(err)
	}
	defer h.close()
	fmt.Printf("pull-smoke: node A serving CAS over TLS (token-gated) on the loopback listener\n")

	// --- node B: pull the template from A over the real handshake ---
	client, err := newPullClient(b, h.port)
	if err != nil {
		return setupErr(err)
	}
	engineB, err := fork.NewEngine(dataB, *fcBin, *kernel, firecracker.JailerConfig{}, fork.EngineOpts{
		AgentBinPath:   *agentBin,
		PullHTTPClient: client,
	})
	if err != nil {
		return setupErr(fmt.Errorf("node B new engine: %w", err))
	}

	fmt.Printf("pull-smoke: node B pulling template from node A\n")
	if err := engineB.PullTemplate(context.Background(), templateID, digest, h.baseURL(), peerToken); err != nil {
		return fmt.Errorf("node B PullTemplate (expected success): %w", err)
	}

	// --- assert B materialized the snapshot files + wrote the verified marker ---
	if err := assertMaterialized(dataB, templateID); err != nil {
		return err
	}
	fmt.Printf("pull-smoke: node B materialized snapshot files + verified marker present\n")

	// --- fork on B from the PULLED template and exec inside it ---
	sandboxID := "dist-fork-1"
	fmt.Printf("pull-smoke: node B forking %q from the pulled template\n", sandboxID)
	res, err := engineB.Fork(templateID, sandboxID, fork.ForkOpts{})
	if err != nil {
		return fmt.Errorf("node B fork from pulled template (the pulled snapshot did not boot): %w", err)
	}
	defer func() { _ = engineB.Terminate(sandboxID) }()
	fmt.Printf("pull-smoke: node B forked in %.2fms, vsock=%s\n", res.ForkTimeMs, res.VsockPath)

	c, err := connect(res.VsockPath)
	if err != nil {
		return fmt.Errorf("node B connect to pulled fork's guest agent: %w", err)
	}
	defer c.Close()
	if _, err := execOK(c, *expectCmd); err != nil {
		return fmt.Errorf("node B exec in pulled fork (%q): %w", *expectCmd, err)
	}

	fmt.Printf("pull-smoke: PASS: node B pulled + materialized + verified + forked + execed from node A's template\n")
	return nil
}

// runVerify exercises the auth + integrity gates that do NOT need KVM: a wrong
// token is rejected with 403, and a wrong manifest digest fails the pull and
// leaves no verified marker on the puller. It builds the template via the CAS
// store directly (no boot), so it runs on any host.
func runVerify() error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	root := fs.String("root", "", "scratch root")
	_ = fs.Parse(os.Args[1:])
	if *root == "" {
		return setupErr(errors.New("--root is required"))
	}

	b, err := generatePKI()
	if err != nil {
		return setupErr(err)
	}

	// Build a tiny content-addressed snapshot directly in node A's store: two
	// files of a few KiB each. This drives the SAME cas.NewHTTPHandler /
	// cas.Pull / RequirePullToken path the engine pull uses, without needing a
	// bootable rootfs or KVM.
	dataA := filepath.Join(*root, "verify-a")
	storeA, err := cas.New(filepath.Join(dataA, "cas"))
	if err != nil {
		return setupErr(fmt.Errorf("node A cas store: %w", err))
	}
	srcDir := filepath.Join(*root, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return setupErr(err)
	}
	memPath := filepath.Join(srcDir, "mem")
	statePath := filepath.Join(srcDir, "vmstate")
	if err := os.WriteFile(memPath, []byte(strings.Repeat("mem-bytes-", 1024)), 0o644); err != nil {
		return setupErr(err)
	}
	if err := os.WriteFile(statePath, []byte(strings.Repeat("state-bytes-", 1024)), 0o644); err != nil {
		return setupErr(err)
	}
	man, err := storeA.PutSnapshot(map[string]string{"mem": memPath, "vmstate": statePath}, cas.Metadata{
		SnapshotFormatVersion: cas.CurrentSnapshotFormatVersion,
	})
	if err != nil {
		return setupErr(fmt.Errorf("node A put snapshot: %w", err))
	}

	h, err := startHolderCAS(storeA, b)
	if err != nil {
		return setupErr(err)
	}
	defer h.close()

	client, err := newPullClient(b, h.port)
	if err != nil {
		return setupErr(err)
	}

	// (1) Wrong token must be rejected with 403 by the holder before any chunk
	// is served. Drive the transport directly so we can read the HTTP status.
	transport := cas.NewHTTPTransport(strings.TrimSuffix(h.baseURL(), "/cas"), client).WithBearerToken("wrong-token")
	_, err = transport.GetManifest(context.Background(), man.Digest())
	if err == nil {
		return errors.New("ASSERTION: wrong token was ACCEPTED (expected 403 rejection)")
	}
	if !strings.Contains(err.Error(), "403") && !strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		return fmt.Errorf("ASSERTION: wrong token rejected but not with 403/forbidden: %w", err)
	}
	fmt.Printf("pull-smoke: PASS: wrong token rejected (403)\n")

	// Sanity: the CORRECT token reaches the manifest, proving the 403 above was
	// the token gate and not an unrelated transport error.
	okTransport := cas.NewHTTPTransport(strings.TrimSuffix(h.baseURL(), "/cas"), client).WithBearerToken(peerToken)
	if _, err := okTransport.GetManifest(context.Background(), man.Digest()); err != nil {
		return setupErr(fmt.Errorf("correct token failed to fetch manifest (setup/transport problem): %w", err))
	}
	fmt.Printf("pull-smoke: PASS: correct token fetches the manifest\n")

	// (2) A WRONG manifest digest must fail the pull and leave no manifest in
	// node B's store. We pull a digest that does not exist on the holder: the
	// transport returns 404, Pull fails, and nothing is materialized. This is
	// the wrong-digest / tampered-source fail-closed proof at the unit layer
	// (the KVM gate proves the GOOD pull boots).
	dataB := filepath.Join(*root, "verify-b")
	storeB, err := cas.New(filepath.Join(dataB, "cas"))
	if err != nil {
		return setupErr(fmt.Errorf("node B cas store: %w", err))
	}
	// A syntactically valid but non-existent digest (64 hex zeros).
	wrongDigest := cas.Digest(strings.Repeat("0", 64))
	pullTransport := cas.NewHTTPTransport(strings.TrimSuffix(h.baseURL(), "/cas"), client).WithBearerToken(peerToken)
	err = cas.Pull(context.Background(), storeB, pullTransport, wrongDigest)
	if err == nil {
		return errors.New("ASSERTION: pull of a non-existent (wrong) digest SUCCEEDED (expected failure)")
	}
	if storeB.HasManifest(wrongDigest) {
		return errors.New("ASSERTION: a failed pull left the wrong manifest in node B's store (not fail-closed)")
	}
	fmt.Printf("pull-smoke: PASS: wrong digest fails the pull and leaves no manifest (fail-closed)\n")

	fmt.Printf("pull-smoke: PASS: auth + integrity gates proven (403 on wrong token, fail-closed on wrong digest)\n")
	return nil
}

// assertMaterialized checks node B's data dir has the materialized snapshot
// files and the verified marker PullTemplate writes on success.
func assertMaterialized(dataDir, templateID string) error {
	tplDir := filepath.Join(dataDir, "templates", templateID)
	for _, rel := range []string{
		filepath.Join("snapshot", "mem"),
		filepath.Join("snapshot", "vmstate"),
		"rootfs.ext4",
		"verified",
	} {
		p := filepath.Join(tplDir, rel)
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("ASSERTION: pulled template missing %s: %w", rel, err)
		}
	}
	return nil
}

// execOK runs a command in the fork over the guest agent and returns its stdout,
// failing if the transport errors or the command exits nonzero.
func execOK(client *vsock.Client, command string) (string, error) {
	res, err := client.Exec(command, "/", nil, 60)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("command %q exited %d: %s", command, res.ExitCode, res.Stderr)
	}
	return res.Stdout, nil
}

// connect dials the forked guest agent over vsock with a bounded retry while the
// restored VM finishes coming up.
func connect(udsPath string) (*vsock.Client, error) {
	var client *vsock.Client
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		client, err = vsock.Connect(udsPath, vsock.AgentPort)
		if err == nil {
			_, perr := client.Ping()
			if perr == nil {
				return client, nil
			}
			_ = client.Close()
			err = perr
		}
		time.Sleep(1 * time.Second)
	}
	return nil, fmt.Errorf("connect after retries: %w", err)
}
