# Control-Plane Auth Implementation Plan (issue #4)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issue #4. mTLS on the controller-forkd gRPC channel with identity-based authorization (only the controller identity may call mutating RPCs), bearer-token auth on the forkd HTTP sandbox API (per-sandbox capability tokens issued at claim time, consumable by the SDK), threat model updated in the same PR.

**Architecture:** A small internal PKI: the controller bootstraps a CA and two leaf certificates as Kubernetes Secrets at startup (idempotent). forkd serves gRPC with TLS and requires client certificates signed by the CA whose SAN identifies the controller; the controller dials with its client certificate and verifies forkd's SAN. The HTTP sandbox API requires `Authorization: Bearer <token>` per sandbox; the controller mints a random token per claim, sends it in the ForkRequest, and publishes it in a claim-owned Secret for SDK consumption. TLS and tokens are default-on in the shipped manifests; programmatic construction without credentials remains possible for tests and local mock mode, and the threat model states exactly that.

**Tech Stack:** Go crypto/tls + crypto/x509 (no external PKI dependency), gRPC TransportCredentials, controller-runtime, protobuf regen, Python SDK.

**Identities (constants, package `internal/pki`):**
- CA secret `mitos-ca`, server secret `mitos-forkd-tls` (DNS SAN `forkd.mitos`), client secret `mitos-controller-tls` (DNS SAN `controller.mitos`), all in the forkd namespace (default `mitos`).
- Client connections set `ServerName: "forkd.mitos"` so one shared server cert works for every node regardless of dialed IP.
- forkd authorizes a peer iff its verified leaf certificate carries DNS SAN `controller.mitos`.

**Context for the implementer:**
- forkd main: `cmd/forkd/main.go` (gRPC :9090 via `grpc.NewServer()`, HTTP :9091 via `daemon.ServeHTTP`). Controller dial: `internal/controller/node_registry.go:GetConnection` (insecure). Discovery dial: `internal/controller/forkd_discovery.go:refreshCapacity` (insecure).
- HTTP API: `internal/daemon/sandbox_api.go` (mux in `Handler()`; every request body carries `sandbox`).
- Fork path: `internal/controller/forkd_client.go:forkOnNode` builds ForkRequest; `internal/daemon/grpc_service.go` Fork handler; `internal/daemon/server.go:Server.Fork`.
- Proto: `proto/forkd.proto`; regen with `make proto` (protoc + plugins installed; PATH may need `$(go env GOPATH)/bin`).
- Claim reconciler: `internal/controller/sandboxclaim_controller.go` (has Secrets RBAC get/list; will need create for token Secrets; ClusterRole in `deploy/controller/deployment.yaml`).
- Manifests: `deploy/daemon/daemonset.yaml`, `deploy/controller/deployment.yaml`.
- Conventions: CLAUDE.md is authoritative. No em/en dashes anywhere. TDD. Explicit-path git add. Secret values and tokens are never logged. Tests: `eval $(~/go/bin/setup-envtest use 1.31 -p env)` for internal/controller; lint with `~/go/bin/golangci-lint run --timeout=5m` plus `GOOS=linux`.

---

### Task 1: `internal/pki` package

**Files:** Create `internal/pki/pki.go`, `internal/pki/pki_test.go`.

- [ ] Failing tests first:

```go
package pki

import (
	"crypto/tls"
	"net"
	"testing"
)

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	ca, err := NewCA("mitos")
	if err != nil {
		t.Fatal(err)
	}
	server, err := ca.Issue(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Issue(ControllerName)
	if err != nil {
		t.Fatal(err)
	}

	serverTLS, err := ServerTLSConfig(server.CertPEM, server.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := ClientTLSConfig(client.CertPEM, client.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}

	lis, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			done <- err
			return
		}
		done <- conn.(*tls.Conn).Handshake()
		conn.Close()
	}()

	conn, err := tls.Dial("tcp", lis.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("mTLS dial: %v", err)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	_ = net.Conn(conn)
}

func TestServerRejectsClientWithoutCert(t *testing.T) {
	ca, _ := NewCA("mitos")
	server, _ := ca.Issue(ServerName)
	serverTLS, _ := ServerTLSConfig(server.CertPEM, server.KeyPEM, ca.CertPEM())

	lis, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		_ = conn.(*tls.Conn).Handshake()
		conn.Close()
	}()

	conf := &tls.Config{InsecureSkipVerify: true} // no client cert
	conn, err := tls.Dial("tcp", lis.Addr().String(), conf)
	if err == nil {
		// handshake may complete lazily; force it
		err = conn.Handshake()
		conn.Close()
	}
	if err == nil {
		t.Fatal("server accepted a client without a certificate")
	}
}

func TestIssueRejectsUnknownName(t *testing.T) {
	ca, _ := NewCA("mitos")
	if _, err := ca.Issue("imposter.mitos"); err == nil {
		t.Fatal("expected issuance restricted to known identities")
	}
}
```

- [ ] Implement `internal/pki/pki.go`:
  - Constants: `ServerName = "forkd.mitos"`, `ControllerName = "controller.mitos"`.
  - `type CA struct{ cert *x509.Certificate; key crypto.Signer; certPEM []byte }`; `NewCA(org string)` makes a 10-year ECDSA P-256 self-signed CA; `CertPEM() []byte`; `LoadCA(certPEM, keyPEM []byte) (*CA, error)`.
  - `type Leaf struct{ CertPEM, KeyPEM []byte }`; `(ca *CA) Issue(dnsName string) (*Leaf, error)`: 2-year ECDSA leaf with the single DNS SAN; reject names other than the two constants (defense against identity sprawl); also export `(ca *CA) KeyPEM() []byte` for persistence.
  - `ServerTLSConfig(certPEM, keyPEM, caPEM []byte) (*tls.Config, error)`: cert + `ClientAuth: tls.RequireAndVerifyClientCert` + ClientCAs pool + MinVersion TLS1.3.
  - `ClientTLSConfig(certPEM, keyPEM, caPEM []byte) (*tls.Config, error)`: cert + RootCAs pool + `ServerName: ServerName` + MinVersion TLS1.3.
  - `PeerDNSName(ctx context.Context) (string, bool)`: extract the verified leaf's first DNS SAN from gRPC peer info (`google.golang.org/grpc/peer`, `credentials.TLSInfo`); returns false when no TLS peer.
- [ ] `go test ./internal/pki/ -count=1` green; lint; commit `feat: internal PKI with mTLS configs and peer identity extraction`.

---

### Task 2: forkd serves mTLS gRPC + identity authorization

**Files:** Modify `cmd/forkd/main.go`, `internal/daemon/grpc_service.go` (or a new interceptor file `internal/daemon/authz.go`), `internal/daemon/grpc_service_test.go` (adjust), create `internal/daemon/authz_test.go`.

- [ ] forkd flags: `--tls-cert`, `--tls-key`, `--tls-ca` (file paths). When all three set: build `pki.ServerTLSConfig`, serve gRPC with `grpc.Creds(credentials.NewTLS(cfg))` and install a unary interceptor `daemon.RequireControllerIdentity` that rejects every RPC whose peer DNS SAN is not `pki.ControllerName` with `codes.Unauthenticated` (no TLS peer) or `codes.PermissionDenied` (wrong identity). When unset: serve insecure and log a single loud warning line `forkd: gRPC is UNAUTHENTICATED; supply --tls-cert/--tls-key/--tls-ca (threat model section 3)`. Mock mode implies nothing here; flags decide.
- [ ] Interceptor in `internal/daemon/authz.go`:

```go
// RequireControllerIdentity rejects RPCs whose mTLS peer is not the
// controller. Installed only when forkd serves TLS.
func RequireControllerIdentity(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	name, ok := pki.PeerDNSName(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	if name != pki.ControllerName {
		return nil, status.Error(codes.PermissionDenied, "peer "+name+" may not call forkd")
	}
	return handler(ctx, req)
}
```

- [ ] TDD via bufconn-with-TLS or localhost TCP TLS (simpler: real localhost listener): test matrix in `authz_test.go`: (a) controller cert succeeds; (b) no client TLS fails Unauthenticated/transport error; (c) a leaf with the SERVER name used as a client cert fails PermissionDenied. Reuse `pki.NewCA` in tests.
- [ ] Commit `feat: forkd gRPC requires controller mTLS identity when TLS is configured`.

---

### Task 3: controller bootstraps the PKI and dials with mTLS

**Files:** Create `internal/controller/pki_bootstrap.go` + test; modify `internal/controller/node_registry.go` (GetConnection), `internal/controller/forkd_discovery.go` (refreshCapacity), `cmd/controller/main.go`, `deploy/controller/deployment.yaml` (RBAC: secrets create/update in the forkd namespace), `deploy/daemon/daemonset.yaml` (mount `mitos-forkd-tls` + CA, pass the three flags).

- [x] `EnsurePKI(ctx, client, namespace string) (*tls.Config, error)`: idempotently get-or-create the three Secrets (`mitos-ca` with cert+key, `mitos-forkd-tls`, `mitos-controller-tls`); on conflict (parallel controllers) re-read. Returns the controller's `*tls.Config` (via `pki.ClientTLSConfig`) directly; the wrapper struct proved unnecessary. TDD with envtest (Secrets in the test namespace; second call returns identical material; tampered partial state gets healed or errors clearly: choose heal-by-recreate-leafs, never recreate the CA if present).
- [x] `NodeRegistry` gains `TLS *tls.Config` field; `NodeInfo` also gains a per-node `TLS *tls.Config` and `GetConnection` prefers node TLS, then registry TLS, then insecure (deviation: node-level TLS keeps the suite's shared testRegistry usable for mixed TLS and insecure fakes; tests and mock paths construct without TLS). Same for `ForkdDiscovery.refreshCapacity` (ForkdDiscovery has a `TLS *tls.Config` field, stamps it onto discovered NodeInfo; both wired from main).
- [x] `cmd/controller/main.go`: after manager construction, run `EnsurePKI` with a direct (non-cached) client before mgr.Start (controller-runtime's mgr.GetClient() cache is not started yet; use `client.New(mgr.GetConfig(), ...)`), set registry/discovery TLS. Flag `--disable-pki-bootstrap` for clusters that bring their own certs.
- [x] Manifests: daemonset mounts secret `mitos-forkd-tls` (cert/key) and `mitos-ca` (ca.crt) as volumes, args gain the three flags; controller ClusterRole gains secrets create/update/patch scoped by resourceNames where possible (create cannot be name-scoped; document why in a comment).
- [x] envtest: claim e2e keeps using StartFakeForkdNode (no TLS) since registry TLS is nil in tests; ADDED `TestClaimReachesReadyOverMTLS` where the fake forkd (StartFakeForkdNodeTLS) serves TLS with pki certs and the registered NodeInfo carries the client config (node-level, not registry-level, so the suite's other insecure fakes keep working), claim still reaches Ready.
- [x] Commit `feat: controller PKI bootstrap and mTLS dialing to forkd`.

---

### Task 4: per-sandbox bearer tokens on the HTTP sandbox API

**Files:** Modify `proto/forkd.proto` (+ regen `make proto`), `internal/daemon/sandbox_api.go`, `internal/daemon/server.go`, `internal/daemon/grpc_service.go`, `internal/controller/forkd_client.go`, `internal/controller/sandboxclaim_controller.go`, `deploy/controller/deployment.yaml` (RBAC secrets create), tests in `internal/daemon/` and `internal/controller/`.

- [x] Proto: `ForkRequest` gains `string api_token = 6;` (and `ForkRunningRequest` gains `string api_token = 5;`). Regenerate; commit the generated code.
- [x] `SandboxAPI`: `RegisterToken(sandboxID, token string)` + enforcement in `Handler()` via a wrapping middleware: every request must carry `Authorization: Bearer <token>`; constant-time compare (`crypto/subtle`) against the token registered for the `sandbox` field in the body; 401 on missing/mismatch; sandboxes registered WITHOUT a token (mock/legacy paths) reject with 401 and reason `no token registered` UNLESS the API was constructed with `AllowTokenless()` (used only by `cmd/sandbox-server` standalone mode and unit tests of other layers; forkd never sets it). Body must be read once: peek the JSON for the sandbox field in the middleware (decode into a small struct from a buffered copy, then hand the buffered body to the real handler).
- [x] `Server.Fork`/`ForkRunning` accept the token (plumb from gRPC request) and call `RegisterToken` after successful fork; `UnregisterSandbox` also clears the token.
- [x] Controller: in the claim reconciler, before forkOnNode, mint `token := rand 32 bytes hex` (crypto/rand); pass through forkOnNode into ForkRequest; after Ready, create (or update) an owned Secret named `<claim-name>-sandbox-token` in the claim namespace with `token` + `endpoint` keys and ownerReference to the claim (GC on claim delete). Never put the token in status, conditions, events, or logs. Fork controller: mint per-fork tokens the same way and create `<forkID>-sandbox-token` Secrets owned by the SandboxFork.
- [x] TDD: daemon test: fork with token, exec via HTTP with the bearer succeeds, without it 401, with a wrong token 401; envtest: claim Ready produces the owned Secret whose token round-trips against the fake forkd's HTTP API (StartFakeForkdNode needs to also start the HTTP handler on a listener; extend the helper).
- [x] Python SDK (`sdk/python/mitos/sandbox.py`): read the token Secret via the k8s API in `_wait_ready` (core v1 read, name `<claim>-sandbox-token`), send `Authorization: Bearer` on every HTTP call; tests with httpx MockTransport asserting the header. The direct-mode client (`direct.py`) is tokenless by design (sandbox-server AllowTokenless); note it in a comment.
- [x] Commit `feat: per-sandbox bearer tokens on the forkd sandbox API`.

---

### Task 5: docs truth pass + verification + PR

- [x] Threat model section 3: controller-forkd row flips to mitigated-when-deployed-as-shipped (mTLS, identity-pinned; programmatic insecure construction remains for tests and is flagged); sandbox HTTP API row flips to mitigated (per-sandbox bearer tokens, constant-time compare, claim-owned Secrets); section 6 plaintext-wire caveat replaced (secrets now transit mTLS when deployed as shipped). Keep honest residuals: token Secrets are readable by anyone with namespace-wide secret read; no rotation yet; jailer still open (issue #2).
- [x] ROADMAP section 1: flip the mTLS line to done with residuals noted.
- [x] Full verification (build, vet, lint darwin+linux, all Go suites with envtest, Python suite, dash grep zero, `git status` clean except intended).
- [x] Push branch `feat/control-plane-auth`, PR titled `Control-plane auth: forkd mTLS identity and sandbox API tokens` with `Closes #4`, watch CI, rerun transient kind-e2e Docker Hub flakes, merge when green per the standing workflow.

**Out of scope:** token rotation; attenuated/macaroon tokens (issue #25 builds on this); jailer (#2); RBAC narrowing of controller secret reads (tracked in threat model section 6).
