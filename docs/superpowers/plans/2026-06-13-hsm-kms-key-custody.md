# HSM/KMS Envelope Key Custody Implementation Plan (issue #31 follow-up)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add envelope encryption to the at-rest key custody (the `#31` documented follow-up "KMS/HSM envelope wrapping"). Today the controller generates a 256-bit data-encryption key (DEK) with `crypto/rand`, stores the RAW DEK in a `<template>-enc-key` Kubernetes Secret, and delivers the raw DEK to forkd over the mTLS gRPC `CreateTemplate`/`Fork` requests (`internal/controller/enc_key_secret.go`, `proto/forkd.proto` field `encryption_key`). The raw DEK is therefore plaintext in etcd, which is the residual we currently disclose as the operator's etcd-encryption-at-rest responsibility. This plan WRAPS the DEK with a key-encryption key (KEK) held by a pluggable KMS: the controller generates a DEK, wraps it with the KEK, stores only the WRAPPED DEK (plus the KEK id) in the Secret, and delivers the wrapped DEK over the RPC. forkd unwraps the DEK via the KMS at container open/build time, hands the plaintext DEK to `storecrypt`, and zeroizes it after use. The plaintext DEK never persists to disk and is never stored in etcd. Crypto-shred is unchanged in spirit: deleting the wrapped-DEK Secret (and the node-side LUKS keyslots) renders the ciphertext unrecoverable; with envelope encryption, an attacker who exfiltrated etcd but not the KEK cannot even unwrap the DEK.

**Honesty constraint:** the plaintext DEK is a secret value. It is generated in controller memory, wrapped immediately, and zeroized; only the wrapped DEK and the KEK id are persisted (Secret/etcd) or logged (the KEK id is not secret; the wrapped DEK is opaque ciphertext but we still never log it). forkd receives the wrapped DEK over mTLS, unwraps it via the KMS into process memory only, uses it, and zeroizes it. The KEK itself never leaves the KMS boundary: for the local provider it is an AES-256 key loaded from a dedicated Secret/file with restrictive permissions (the dev/CI custody); for a future cloud KMS provider the KEK never leaves the HSM at all (wrap/unwrap are remote calls). We pick a LOCAL software KEK provider as the default and the only provider shipped here, because it is testable in CI with NO cloud credentials. Cloud KMS (AWS KMS, GCP KMS, Vault Transit) providers are an explicitly documented follow-up: the interface is shaped for them (context-bound `Wrap`/`Unwrap` returning a KEK id, errors carrying remediation), but no cloud SDK is added in this PR. We update `docs/encryption.md` and `docs/threat-model.md` in the same PR.

**Architecture:**

- A new `internal/kms` package defines the pluggable interface:
  ```go
  type Wrapper interface {
      Wrap(ctx context.Context, plaintextDEK []byte) (WrappedKey, error)
      Unwrap(ctx context.Context, w WrappedKey) ([]byte, error)
      KEKID() string
  }
  type WrappedKey struct {
      KEKID      string // identifies the KEK that wrapped this DEK; not secret
      Ciphertext []byte // the wrapped DEK; opaque, never logged
  }
  ```
  The first and only concrete provider is `LocalKEK`: AES-256-GCM with a 32-byte KEK and a random 12-byte nonce per wrap, output framed as `nonce || GCM(ciphertext+tag)`. `KEKID()` returns a stable non-secret label (`local:` + a short fingerprint of the KEK, e.g. the first bytes of `SHA-256(KEK)` hex) so a wrapped DEK can be matched to its KEK and a KEK rotation is detectable.

- The controller side: `enc_key_secret.go` swaps raw-DEK custody for envelope custody. `EnsureEncKey` generates a DEK with `crypto/rand`, calls `kms.Wrap`, zeroizes the plaintext DEK, and stores the WRAPPED DEK in the Secret under data keys `wrapped-dek` and `kek-id` (the old `key` data key is no longer written). It returns the wrapped DEK bytes + KEK id (NOT the plaintext DEK) to the caller. The RPC field `encryption_key` now carries the WRAPPED DEK; a new sibling field `kek_id` carries the KEK id so forkd can select the matching KMS/KEK. The controller never unwraps; it never sees the plaintext DEK after `EnsureEncKey` returns.

- The forkd side: a `kms.Wrapper` is constructed behind `--enable-encryption` from a KEK file flag (`--kek-file`, the local provider). The `RequestKeyProvider` (`internal/fork/encryption.go`) is extended so the daemon stashes the WRAPPED DEK + KEK id from the request; `KeyFor(scopeID)` UNWRAPS via the `kms.Wrapper` on demand into a fresh `storecrypt.Key`, returns it for the cryptsetup call, and the engine zeroizes it after the open/create. `ForgetKey` zeroizes any cached plaintext and drops the wrapped entry. The plaintext DEK exists in forkd memory only for the duration of an open/create and is zeroized immediately after.

- Crypto-shred: unchanged path plus a stronger property. Deleting the `<template>-enc-key` Secret destroys the only stored copy of the wrapped DEK; `luksErase` wipes the node keyslots; the in-memory plaintext DEK is zeroized. With envelope encryption, even an etcd backup that still holds the wrapped DEK is useless without the KEK, and rotating/destroying the KEK crypto-shreds every DEK it wrapped at once.

**Context for the implementer:**

- Existing custody code to MODIFY, not rebuild:
  - `internal/controller/enc_key_secret.go`: `EnsureEncKey(ctx, c, ns, templateID, owner) ([]byte, error)` creates/reads the `<template>-enc-key` Secret holding raw key bytes under data key `key` (`encKeyDataKey`), `encKeyLen = 32`. `DeleteEncKey` deletes the Secret. Callers: `internal/controller/sandboxpool_controller.go:272` (`ensureTemplateBuilt`) and `internal/controller/sandboxclaim_controller.go:432`. The returned bytes flow into `CreateTemplateRequest.EncryptionKey` (`sandboxpool_controller.go:367`) and `ForkRequest.EncryptionKey` (`forkd_client.go:101`).
  - `internal/fork/encryption.go`: `KeyProvider{ KeyFor(scopeID)(storecrypt.Key,error); ForgetKey(scopeID) }`, `RequestKeyProvider{ SetKey, KeyFor, ForgetKey }` holding `map[string]storecrypt.Key`. The engine calls `KeyFor` in `createTemplateContainer` and `ensureTemplateOpen`, and `ForgetKey` in `shredTemplateContainer`. `storecrypt.Key` is `[]byte` with `Zeroize()` and a redacting `String()`/`MarshalText()`.
  - `internal/daemon/grpc_service.go:45` and `:132`: `if len(req.EncryptionKey) > 0 && g.srv.keyProvider != nil { keyProvider.SetKey(scopeID, storecrypt.Key(req.EncryptionKey)); defer ForgetKey(scopeID) }`. `server.go` exposes `SetKeyProvider(RequestKeyStasher)`; `RequestKeyStasher` is the `SetKey`/`ForgetKey` seam.
  - `cmd/forkd/main.go:203`: `reqKeyProvider = fork.NewRequestKeyProvider()` behind `--enable-encryption`, wired into `EngineOpts.KeyProvider` and `server.SetKeyProvider`.
  - `proto/forkd.proto`: `CreateTemplateRequest.encryption_key = 7` (bytes), `ForkRequest.encryption_key = 8` (bytes). Regen with `make proto`.

- Controller Secret RBAC already grants get;list;watch;create;update;delete on secrets (the enc-key Secret is created/deleted today). No new RBAC verb is required; we only add data keys to the same Secret.

- The local-provider KEK Secret on the CONTROLLER is out of band of the per-template Secret: the controller reads its KEK from a mounted Secret/file given by an operator flag (`--kek-file`), the SAME bytes the forkd `--kek-file` points at (in dev/CI a shared volume; in production a follow-up cloud KMS removes the need to co-locate the KEK). The KEK is a secret value: never logged, never in argv, never in an error message; only its `KEKID()` fingerprint (non-secret) is logged.

- Conventions: CLAUDE.md is authoritative. No em or en dashes anywhere. TDD: failing test first, in the same commit as the behavior. Explicit-path `git add` only, never `git add -A`. Conventional commits ending with the trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. Lint clean BOTH `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`. Octal as `0o600`. Error wrapping `fmt.Errorf("context: %w", err)`. The plaintext DEK and the KEK are secret VALUES: never logged, never in argv, never on the node data disk.

---

## File Structure

New files:

- `internal/kms/kms.go` : the `Wrapper` interface, `WrappedKey` type, package doc on the secret discipline.
- `internal/kms/local.go` : `LocalKEK` AES-256-GCM provider, `NewLocalKEK([]byte) (*LocalKEK, error)`, `LoadLocalKEKFromFile(path) (*LocalKEK, error)`.
- `internal/kms/kms_test.go` : interface-level round-trip + tamper tests against `LocalKEK`.
- `internal/kms/local_test.go` : `LocalKEK` unit tests (wrong-length KEK, nonce uniqueness, KEKID stability, file loader).

Modified files:

- `proto/forkd.proto` (+ generated `internal/forkdpb/*.pb.go` via `make proto`) : add `kek_id` field to `CreateTemplateRequest` and `ForkRequest`; redocument `encryption_key` as the WRAPPED DEK.
- `internal/fork/encryption.go` : `RequestKeyProvider` holds wrapped DEK + KEK id; unwraps via an injected `kms.Wrapper` in `KeyFor`; zeroizes plaintext after use.
- `internal/fork/engine.go` (`EngineOpts`) : add an optional `KMS kms.Wrapper` field passed to the provider.
- `internal/daemon/grpc_service.go` : stash `req.EncryptionKey` (wrapped) + `req.KekId` via the extended `SetWrappedKey` seam.
- `internal/daemon/server.go` : extend `RequestKeyStasher` for the wrapped form.
- `cmd/forkd/main.go` : `--kek-file` flag; construct `kms.LoadLocalKEKFromFile`; wire into the provider; fail closed if `--enable-encryption` is set without a KEK.
- `internal/controller/enc_key_secret.go` : envelope custody (generate DEK, wrap, store wrapped); a `kms.Wrapper` field on the reconcilers.
- `internal/controller/sandboxpool_controller.go`, `internal/controller/sandboxclaim_controller.go`, `internal/controller/forkd_client.go` : carry the wrapped DEK + KEK id into the RPCs.
- `cmd/controller/main.go` : `--kek-file` flag; construct the controller-side `kms.LocalKEK`; inject into the reconcilers.
- `internal/controller/enc_key_envtest_test.go` : assert the Secret stores `wrapped-dek` + `kek-id`, NOT a raw `key`, and the RPC carries the wrapped DEK + KEK id.
- `docs/encryption.md`, `docs/threat-model.md`, `ROADMAP.md` : update for envelope custody.

---

### Task 1: the KMS interface and the local AES-GCM KEK provider

**Files:** `internal/kms/kms.go` (new), `internal/kms/local.go` (new), `internal/kms/kms_test.go` (new), `internal/kms/local_test.go` (new).

- [ ] Write the failing test first. Create `internal/kms/local_test.go`:
  ```go
  package kms

  import (
  	"bytes"
  	"context"
  	"crypto/rand"
  	"strings"
  	"testing"
  )

  func TestLocalKEKWrapUnwrapRoundTrip(t *testing.T) {
  	kek := make([]byte, 32)
  	if _, err := rand.Read(kek); err != nil {
  		t.Fatalf("rand: %v", err)
  	}
  	w, err := NewLocalKEK(kek)
  	if err != nil {
  		t.Fatalf("NewLocalKEK: %v", err)
  	}
  	dek := []byte("0123456789abcdef0123456789abcdef") // 32-byte DEK
  	wrapped, err := w.Wrap(context.Background(), dek)
  	if err != nil {
  		t.Fatalf("Wrap: %v", err)
  	}
  	if bytes.Contains(wrapped.Ciphertext, dek) {
  		t.Fatal("wrapped DEK contains the plaintext DEK")
  	}
  	if wrapped.KEKID != w.KEKID() {
  		t.Fatalf("KEKID mismatch: %q vs %q", wrapped.KEKID, w.KEKID())
  	}
  	got, err := w.Unwrap(context.Background(), wrapped)
  	if err != nil {
  		t.Fatalf("Unwrap: %v", err)
  	}
  	if !bytes.Equal(got, dek) {
  		t.Fatal("round-trip DEK mismatch")
  	}
  }

  func TestLocalKEKRejectsWrongKEKLength(t *testing.T) {
  	if _, err := NewLocalKEK(make([]byte, 16)); err == nil {
  		t.Fatal("expected error for 16-byte KEK")
  	}
  }

  func TestLocalKEKUnwrapTamperFails(t *testing.T) {
  	kek := make([]byte, 32)
  	_, _ = rand.Read(kek)
  	w, _ := NewLocalKEK(kek)
  	wrapped, _ := w.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef"))
  	wrapped.Ciphertext[len(wrapped.Ciphertext)-1] ^= 0xff
  	if _, err := w.Unwrap(context.Background(), wrapped); err == nil {
  		t.Fatal("expected GCM auth failure on tampered ciphertext")
  	}
  }

  func TestLocalKEKIDIsStableAndNonSecret(t *testing.T) {
  	kek := bytes.Repeat([]byte{7}, 32)
  	w, _ := NewLocalKEK(kek)
  	id := w.KEKID()
  	if !strings.HasPrefix(id, "local:") {
  		t.Fatalf("KEKID %q missing local: prefix", id)
  	}
  	if strings.Contains(id, string(kek)) {
  		t.Fatal("KEKID leaks KEK bytes")
  	}
  	w2, _ := NewLocalKEK(kek)
  	if w2.KEKID() != id {
  		t.Fatal("KEKID not stable for the same KEK")
  	}
  }
  ```
- [ ] Run `go test ./internal/kms/` and confirm it fails to compile (package does not exist yet).
- [ ] Implement `internal/kms/kms.go`:
  ```go
  // Package kms provides envelope encryption for the at-rest data-encryption key
  // (DEK). A DEK is wrapped (encrypted) by a key-encryption key (KEK) that lives
  // behind a Wrapper. Only the WRAPPED DEK is persisted (in a Kubernetes Secret)
  // and carried over the wire; the plaintext DEK exists in memory only while a
  // cryptsetup operation needs it and is zeroized after. The KEK never leaves the
  // Wrapper boundary: the local provider holds it in process memory; a future
  // cloud KMS provider keeps it in the HSM and performs wrap/unwrap remotely.
  //
  // Secret discipline: the plaintext DEK and the KEK are secret VALUES. They are
  // never logged, never placed in an error message, and never written to a host
  // path. The KEK id (KEKID) is NOT secret: it is a stable, non-reversible label
  // used to match a wrapped DEK to the KEK that produced it and is safe to log.
  package kms

  import "context"

  // WrappedKey is a DEK encrypted under a KEK. KEKID identifies the KEK that
  // produced it (safe to log); Ciphertext is the opaque wrapped DEK (never
  // logged, even though it is not the plaintext).
  type WrappedKey struct {
  	KEKID      string
  	Ciphertext []byte
  }

  // Wrapper performs envelope wrap/unwrap of a DEK under a KEK. Implementations
  // must be safe for concurrent use. Wrap and Unwrap take a context so a cloud
  // KMS provider can bound and cancel its remote call; the local provider ignores
  // it. Errors should carry actionable remediation text (the API v2 LLM-legible
  // error rule) and must never include the plaintext DEK or the KEK.
  type Wrapper interface {
  	Wrap(ctx context.Context, plaintextDEK []byte) (WrappedKey, error)
  	Unwrap(ctx context.Context, w WrappedKey) ([]byte, error)
  	KEKID() string
  }
  ```
- [ ] Implement `internal/kms/local.go`:
  ```go
  package kms

  import (
  	"context"
  	"crypto/aes"
  	"crypto/cipher"
  	"crypto/rand"
  	"crypto/sha256"
  	"encoding/hex"
  	"fmt"
  	"io"
  	"os"
  )

  // kekLen is the local KEK length in bytes (AES-256).
  const kekLen = 32

  // LocalKEK wraps a DEK with AES-256-GCM under a process-held KEK. It is the
  // dev/CI provider: the KEK comes from a Kubernetes Secret mounted as a file,
  // not from a cloud HSM. It is safe for concurrent use (the cipher.AEAD is
  // stateless and a fresh nonce is drawn per Wrap).
  type LocalKEK struct {
  	aead  cipher.AEAD
  	kekID string
  }

  // NewLocalKEK builds a provider from a 32-byte AES-256 KEK. The KEK is a secret
  // value: it is never logged or placed in an error. The returned KEKID is a
  // non-reversible fingerprint, safe to log.
  func NewLocalKEK(kek []byte) (*LocalKEK, error) {
  	if len(kek) != kekLen {
  		return nil, fmt.Errorf("local KEK must be %d bytes (AES-256); got %d: supply a 32-byte key via --kek-file", kekLen, len(kek))
  	}
  	block, err := aes.NewCipher(kek)
  	if err != nil {
  		return nil, fmt.Errorf("init AES cipher for local KEK: %w", err)
  	}
  	aead, err := cipher.NewGCM(block)
  	if err != nil {
  		return nil, fmt.Errorf("init GCM for local KEK: %w", err)
  	}
  	sum := sha256.Sum256(kek)
  	return &LocalKEK{aead: aead, kekID: "local:" + hex.EncodeToString(sum[:8])}, nil
  }

  // LoadLocalKEKFromFile reads a 32-byte KEK from path. The file must hold exactly
  // 32 raw bytes (e.g. a Kubernetes Secret key mounted as a file). The bytes are a
  // secret value: this never logs them and zeroizes the read buffer after use.
  func LoadLocalKEKFromFile(path string) (*LocalKEK, error) {
  	b, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied KEK file
  	if err != nil {
  		return nil, fmt.Errorf("read KEK file %s: %w", path, err)
  	}
  	w, werr := NewLocalKEK(b)
  	for i := range b {
  		b[i] = 0
  	}
  	if werr != nil {
  		return nil, werr
  	}
  	return w, nil
  }

  // Wrap encrypts the plaintext DEK with AES-256-GCM under the KEK. The output is
  // nonce || ciphertext+tag. A fresh random nonce is drawn per call. The plaintext
  // DEK is never logged or placed in an error.
  func (l *LocalKEK) Wrap(_ context.Context, plaintextDEK []byte) (WrappedKey, error) {
  	nonce := make([]byte, l.aead.NonceSize())
  	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
  		return WrappedKey{}, fmt.Errorf("draw GCM nonce: %w", err)
  	}
  	ct := l.aead.Seal(nonce, nonce, plaintextDEK, nil)
  	return WrappedKey{KEKID: l.kekID, Ciphertext: ct}, nil
  }

  // Unwrap decrypts a wrapped DEK. It returns an error (never the plaintext) on a
  // KEK mismatch, a short ciphertext, or a GCM authentication failure.
  func (l *LocalKEK) Unwrap(_ context.Context, w WrappedKey) ([]byte, error) {
  	if w.KEKID != l.kekID {
  		return nil, fmt.Errorf("wrapped DEK was produced by KEK %q but this node holds KEK %q: deliver the matching KEK (check --kek-file) or rewrap the DEK", w.KEKID, l.kekID)
  	}
  	ns := l.aead.NonceSize()
  	if len(w.Ciphertext) < ns {
  		return nil, fmt.Errorf("wrapped DEK too short: %d bytes, want at least %d", len(w.Ciphertext), ns)
  	}
  	nonce, ct := w.Ciphertext[:ns], w.Ciphertext[ns:]
  	dek, err := l.aead.Open(nil, nonce, ct, nil)
  	if err != nil {
  		return nil, fmt.Errorf("unwrap DEK: GCM authentication failed (wrong KEK or corrupted wrapped DEK): %w", err)
  	}
  	return dek, nil
  }

  // KEKID returns the non-secret KEK fingerprint, safe to log.
  func (l *LocalKEK) KEKID() string { return l.kekID }
  ```
- [ ] Add `internal/kms/kms_test.go` asserting `LocalKEK` satisfies `Wrapper` and a `LoadLocalKEKFromFile` round-trip from a temp file:
  ```go
  package kms

  import (
  	"context"
  	"os"
  	"path/filepath"
  	"testing"
  )

  var _ Wrapper = (*LocalKEK)(nil)

  func TestLoadLocalKEKFromFileRoundTrip(t *testing.T) {
  	dir := t.TempDir()
  	path := filepath.Join(dir, "kek")
  	kek := make([]byte, 32)
  	for i := range kek {
  		kek[i] = byte(i)
  	}
  	if err := os.WriteFile(path, kek, 0o600); err != nil {
  		t.Fatalf("write kek: %v", err)
  	}
  	w, err := LoadLocalKEKFromFile(path)
  	if err != nil {
  		t.Fatalf("LoadLocalKEKFromFile: %v", err)
  	}
  	wrapped, err := w.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef"))
  	if err != nil {
  		t.Fatalf("Wrap: %v", err)
  	}
  	if _, err := w.Unwrap(context.Background(), wrapped); err != nil {
  		t.Fatalf("Unwrap: %v", err)
  	}
  }

  func TestLoadLocalKEKFromFileWrongLength(t *testing.T) {
  	dir := t.TempDir()
  	path := filepath.Join(dir, "kek")
  	if err := os.WriteFile(path, make([]byte, 10), 0o600); err != nil {
  		t.Fatalf("write: %v", err)
  	}
  	if _, err := LoadLocalKEKFromFile(path); err == nil {
  		t.Fatal("expected error for 10-byte KEK file")
  	}
  }
  ```
- [ ] Run `go test ./internal/kms/` and confirm green.
- [ ] Run `gofmt -l internal/kms/`, `golangci-lint run --timeout=5m ./internal/kms/...`, and `GOOS=linux golangci-lint run --timeout=5m ./internal/kms/...`; fix any finding.
- [ ] `git add internal/kms/kms.go internal/kms/local.go internal/kms/kms_test.go internal/kms/local_test.go`
- [ ] Commit `feat: add pluggable KMS Wrapper with a local AES-256-GCM KEK provider`.

### Task 2: proto carries the wrapped DEK plus a KEK id

**Files:** `proto/forkd.proto` (+ regen `internal/forkdpb`).

- [ ] In `proto/forkd.proto`, redocument `CreateTemplateRequest.encryption_key = 7` to say it now carries the WRAPPED DEK (opaque ciphertext, never logged) and add a sibling field:
  ```proto
    // encryption_key is the per-template at-rest DEK, WRAPPED by the KMS KEK
    // (envelope encryption). It is opaque ciphertext, not the plaintext key, but
    // is still a SECRET-adjacent value carried only over the mTLS RPC and MUST
    // NEVER be logged or written to the node data disk. The node unwraps it via
    // its KMS at container open/build time and zeroizes the plaintext after.
    // Empty means the template is not encrypted.
    bytes encryption_key = 7;
    // kek_id identifies the KMS KEK that wrapped encryption_key, so the node can
    // select the matching KEK to unwrap. It is NOT secret and may be logged.
    // Empty when the template is not encrypted.
    string kek_id = 8;
  ```
- [ ] In `ForkRequest`, redocument `encryption_key = 8` likewise and add `string kek_id = 9;` with the same comment.
- [ ] Run `make proto`. Confirm `internal/forkdpb` now has `KekId` getters on both messages.
- [ ] Run `go build ./...` to confirm the regenerated code compiles.
- [ ] Run `gofmt -l proto/ internal/forkdpb/` (should be empty; generated code is gofmt-clean by protoc-gen-go).
- [ ] `git add proto/forkd.proto internal/forkdpb/` (explicit paths for the proto and the regenerated stubs).
- [ ] Commit `feat: proto carries the wrapped DEK and its KEK id`.

### Task 3: forkd unwraps the wrapped DEK via the KMS and zeroizes it

**Files:** `internal/fork/encryption.go`, `internal/fork/engine.go` (`EngineOpts`), `internal/fork/encryption_test.go`.

- [ ] Write the failing test first. Add to `internal/fork/encryption_test.go`:
  ```go
  func TestRequestKeyProviderUnwrapsViaKMS(t *testing.T) {
  	kek := make([]byte, 32)
  	for i := range kek {
  		kek[i] = byte(i + 1)
  	}
  	w, err := kms.NewLocalKEK(kek)
  	if err != nil {
  		t.Fatalf("NewLocalKEK: %v", err)
  	}
  	dek := make([]byte, 32)
  	for i := range dek {
  		dek[i] = byte(i)
  	}
  	wrapped, err := w.Wrap(context.Background(), dek)
  	if err != nil {
  		t.Fatalf("Wrap: %v", err)
  	}
  	p := NewRequestKeyProvider(w)
  	p.SetWrappedKey("tmpl", wrapped.Ciphertext, wrapped.KEKID)
  	got, err := p.KeyFor("tmpl")
  	if err != nil {
  		t.Fatalf("KeyFor: %v", err)
  	}
  	if !bytes.Equal(got, dek) {
  		t.Fatal("unwrapped DEK mismatch")
  	}
  	p.ForgetKey("tmpl")
  	if _, err := p.KeyFor("tmpl"); err == nil {
  		t.Fatal("expected fail-closed after ForgetKey")
  	}
  }

  func TestRequestKeyProviderFailsClosedWithNoWrappedKey(t *testing.T) {
  	w, _ := kms.NewLocalKEK(make([]byte, 32))
  	p := NewRequestKeyProvider(w)
  	if _, err := p.KeyFor("missing"); err == nil {
  		t.Fatal("expected error when no wrapped key is stashed")
  	}
  }

  func TestRequestKeyProviderRejectsWrongKEK(t *testing.T) {
  	kekA := make([]byte, 32)
  	kekB := make([]byte, 32)
  	for i := range kekB {
  		kekB[i] = 0xaa
  	}
  	wa, _ := kms.NewLocalKEK(kekA)
  	wb, _ := kms.NewLocalKEK(kekB)
  	wrapped, _ := wa.Wrap(context.Background(), make([]byte, 32))
  	p := NewRequestKeyProvider(wb) // node holds the wrong KEK
  	p.SetWrappedKey("tmpl", wrapped.Ciphertext, wrapped.KEKID)
  	if _, err := p.KeyFor("tmpl"); err == nil {
  		t.Fatal("expected unwrap error for KEK mismatch")
  	}
  }
  ```
  Add the imports `"bytes"`, `"context"`, and `"github.com/paperclipinc/mitos/internal/kms"` to the test file if absent.
- [ ] Run `go test ./internal/fork/ -run TestRequestKeyProvider` and confirm it fails to compile (`NewRequestKeyProvider` takes no arg today; `SetWrappedKey` does not exist).
- [ ] Edit `internal/fork/encryption.go`. Replace the `RequestKeyProvider` definition so it holds the WRAPPED DEK + KEK id and an injected `kms.Wrapper`:
  ```go
  // RequestKeyProvider holds per-scope WRAPPED DEKs delivered by the controller
  // over the mTLS RPC and unwraps them via the KMS at use time (envelope
  // encryption). The daemon stashes the request-supplied wrapped DEK with
  // SetWrappedKey before invoking the engine and ForgetKey after. KeyFor unwraps
  // the DEK into a fresh storecrypt.Key via the KMS; the engine zeroizes that key
  // after the cryptsetup call so the plaintext DEK does not linger. KeyFor on a
  // scope with no stashed wrapped DEK returns an error so encryption FAILS CLOSED.
  // The plaintext DEK is a secret value: it is never logged and the storecrypt.Key
  // type exposes no leaking String(). Safe for concurrent use.
  type RequestKeyProvider struct {
  	kms     kms.Wrapper
  	mu      sync.Mutex
  	wrapped map[string]wrappedDEK
  }

  // wrappedDEK is the per-scope wrapped key material the controller delivered.
  type wrappedDEK struct {
  	ciphertext []byte
  	kekID      string
  }

  // NewRequestKeyProvider returns a provider that unwraps DEKs via w. w must be
  // non-nil whenever encryption is enabled.
  func NewRequestKeyProvider(w kms.Wrapper) *RequestKeyProvider {
  	return &RequestKeyProvider{kms: w, wrapped: make(map[string]wrappedDEK)}
  }

  // SetWrappedKey stashes the wrapped DEK and its KEK id for scopeID for the
  // duration of the operation. The daemon calls it before invoking the engine.
  // The wrapped DEK is opaque ciphertext and is never logged.
  func (p *RequestKeyProvider) SetWrappedKey(scopeID string, ciphertext []byte, kekID string) {
  	p.mu.Lock()
  	defer p.mu.Unlock()
  	p.wrapped[scopeID] = wrappedDEK{ciphertext: ciphertext, kekID: kekID}
  }

  // KeyFor unwraps the stashed wrapped DEK for scopeID via the KMS and returns the
  // plaintext DEK as a storecrypt.Key. It returns an error when no wrapped DEK is
  // stashed (fail closed) or when the KMS cannot unwrap (wrong KEK, corruption).
  // The error names only the scope and the non-secret KEK id, never key material.
  func (p *RequestKeyProvider) KeyFor(scopeID string) (storecrypt.Key, error) {
  	p.mu.Lock()
  	wd, ok := p.wrapped[scopeID]
  	p.mu.Unlock()
  	if !ok {
  		return nil, fmt.Errorf("no encryption key available for scope %s: the request must carry the wrapped DEK over mTLS", scopeID)
  	}
  	if p.kms == nil {
  		return nil, fmt.Errorf("scope %s has a wrapped DEK but no KMS is configured to unwrap it", scopeID)
  	}
  	dek, err := p.kms.Unwrap(context.Background(), kms.WrappedKey{KEKID: wd.kekID, Ciphertext: wd.ciphertext})
  	if err != nil {
  		return nil, fmt.Errorf("unwrap DEK for scope %s (kek %s): %w", scopeID, wd.kekID, err)
  	}
  	return storecrypt.Key(dek), nil
  }

  // ForgetKey drops the stashed wrapped DEK for scopeID. The plaintext DEK is not
  // held here (KeyFor returns a fresh copy the engine zeroizes), so there is no
  // plaintext to wipe; the wrapped ciphertext is opaque but we still drop it.
  func (p *RequestKeyProvider) ForgetKey(scopeID string) {
  	p.mu.Lock()
  	defer p.mu.Unlock()
  	delete(p.wrapped, scopeID)
  }
  ```
  Add `"github.com/paperclipinc/mitos/internal/kms"` to the import block.
- [ ] The engine must zeroize the plaintext DEK after each cryptsetup call (since `KeyFor` now returns a freshly-unwrapped copy, not a cached one). In `internal/fork/encryption.go`, in `createTemplateContainer` add `defer key.Zeroize()` immediately after the `KeyFor` call (before `crypt.Create`), and in `ensureTemplateOpen` add `defer key.Zeroize()` immediately after its `KeyFor` call. Confirm `shredTemplateContainer` still calls `keyProvider.ForgetKey(id)` (now dropping the wrapped entry).
- [ ] Add an optional `KMS kms.Wrapper` field to `EngineOpts` in `internal/fork/engine.go` with a doc comment ("the unwrapper for envelope-encrypted DEKs; required when EnableEncryption and a RequestKeyProvider are used") and store it on the `Engine` if the engine needs it directly (it does not unwrap itself; the provider does, so this field may be informational only. If unused, omit it and inject the KMS solely via `NewRequestKeyProvider` in `cmd/forkd`.). Prefer injecting via the provider only: do NOT add an unused `EngineOpts.KMS` field if the linter would flag it.
- [ ] Run `go test ./internal/fork/ -run TestRequestKeyProvider` and confirm green. Run the full `go test ./internal/fork/` and fix any caller of `NewRequestKeyProvider()` inside fork tests to pass a `kms.NewLocalKEK(make([]byte,32))`-backed provider.
- [ ] Run `gofmt -l internal/fork/`, `golangci-lint run --timeout=5m ./internal/fork/...`, and `GOOS=linux golangci-lint run --timeout=5m ./internal/fork/...`.
- [ ] `git add internal/fork/encryption.go internal/fork/encryption_test.go internal/fork/engine.go`
- [ ] Commit `feat: forkd unwraps the wrapped DEK via the KMS and zeroizes the plaintext`.

### Task 4: daemon stashes the wrapped DEK plus KEK id from the request

**Files:** `internal/daemon/server.go` (`RequestKeyStasher`), `internal/daemon/grpc_service.go`, `internal/daemon/enc_key_test.go`.

- [ ] Write the failing test first. In `internal/daemon/enc_key_test.go`, add a test asserting the CreateTemplate and Fork handlers call `SetWrappedKey(scopeID, req.EncryptionKey, req.KekId)` and `ForgetKey` after, using a fake stasher that records the wrapped bytes + KEK id (presence, never logging the value). Model it on the existing stasher fake in that file; add a recorded `kekID` field and assert it matches the request's `KekId`.
- [ ] Run `go test ./internal/daemon/ -run EncKey` and confirm it fails (the `RequestKeyStasher` has no `SetWrappedKey`).
- [ ] In `internal/daemon/server.go`, change the `RequestKeyStasher` seam from `SetKey(scopeID string, key storecrypt.Key)` to:
  ```go
  // RequestKeyStasher is the seam the gRPC handlers use to hand the controller-
  // delivered WRAPPED DEK to the engine's key provider for the duration of a
  // CreateTemplate/Fork call. The same *fork.RequestKeyProvider satisfies it.
  type RequestKeyStasher interface {
  	SetWrappedKey(scopeID string, wrappedDEK []byte, kekID string)
  	ForgetKey(scopeID string)
  }
  ```
  Remove the now-unused `storecrypt` import from `server.go` if it becomes unused.
- [ ] In `internal/daemon/grpc_service.go`, update both stash sites. For CreateTemplate (around line 132):
  ```go
  if len(req.EncryptionKey) > 0 && g.srv.keyProvider != nil {
  	// The request carries the WRAPPED DEK and its KEK id over mTLS. Neither the
  	// wrapped DEK nor the (eventual) plaintext is ever logged or recorded as a
  	// span attribute. The engine unwraps via the KMS and zeroizes the plaintext.
  	g.srv.keyProvider.SetWrappedKey(req.TemplateId, req.EncryptionKey, req.KekId)
  	defer g.srv.keyProvider.ForgetKey(req.TemplateId)
  }
  ```
  And the same for Fork (around line 45) using `req.SnapshotId`. Remove the `storecrypt.Key(...)` conversion (the seam now takes raw wrapped bytes). Drop the `storecrypt` import from `grpc_service.go` if it becomes unused.
- [ ] Run `go test ./internal/daemon/` and confirm green; fix any other caller of the old `SetKey` seam in daemon tests.
- [ ] Run `gofmt -l internal/daemon/`, `golangci-lint run --timeout=5m ./internal/daemon/...`, and `GOOS=linux golangci-lint run --timeout=5m ./internal/daemon/...`.
- [ ] `git add internal/daemon/server.go internal/daemon/grpc_service.go internal/daemon/enc_key_test.go`
- [ ] Commit `feat: daemon stashes the wrapped DEK and KEK id from the mTLS request`.

### Task 5: forkd constructs the local KMS from --kek-file and fails closed without it

**Files:** `cmd/forkd/main.go`.

- [ ] Add a `--kek-file` string flag near the other encryption flags:
  ```go
  flag.StringVar(&kekFile, "kek-file", "", "Path to the 32-byte AES-256 KEK file (mounted from a Kubernetes Secret) used to UNWRAP the per-template DEK delivered over the mTLS RPC (envelope encryption). REQUIRED with --enable-encryption. The KEK is a secret value: it is never logged. Cloud KMS providers (AWS/GCP/Vault) are a documented follow-up.")
  ```
- [ ] In the `if enableEncryption {` block (around line 194), build the local KMS BEFORE constructing the provider and fail closed if absent:
  ```go
  if kekFile == "" {
  	fmt.Fprintln(os.Stderr, "forkd: --enable-encryption requires --kek-file (the KEK that unwraps the per-template DEK); refusing to start so a wrapped DEK can never arrive without an unwrapper")
  	os.Exit(1)
  }
  wrapper, kerr := kms.LoadLocalKEKFromFile(kekFile)
  if kerr != nil {
  	fmt.Fprintf(os.Stderr, "forkd: load KEK: %v\n", kerr)
  	os.Exit(1)
  }
  engineOpts.EnableEncryption = true
  reqKeyProvider = fork.NewRequestKeyProvider(wrapper)
  engineOpts.KeyProvider = reqKeyProvider
  fmt.Printf("forkd: at-rest snapshot encryption ENABLED (envelope: the per-template DEK arrives WRAPPED over the mTLS RPC and is unwrapped by the local KEK %s; the plaintext DEK is never generated or persisted on the node)\n", wrapper.KEKID())
  ```
  Add `"github.com/paperclipinc/mitos/internal/kms"` to the imports and declare `var kekFile string`.
- [ ] Run `go build ./cmd/forkd/` and confirm it compiles.
- [ ] Run `gofmt -l cmd/forkd/`, `golangci-lint run --timeout=5m ./cmd/forkd/...`, and `GOOS=linux golangci-lint run --timeout=5m ./cmd/forkd/...`.
- [ ] `git add cmd/forkd/main.go`
- [ ] Commit `feat: forkd loads the local KEK from --kek-file and fails closed without it`.

### Task 6: controller generates a DEK, wraps it, stores only the wrapped DEK

**Files:** `internal/controller/enc_key_secret.go`, `internal/controller/enc_key_envtest_test.go`.

- [ ] Write the failing test first. In `internal/controller/enc_key_envtest_test.go`, change the existing assertions (or add a new test) to expect the Secret to hold data keys `wrapped-dek` (non-empty) and `kek-id` (matching the injected KMS `KEKID()`), and to NOT hold a `key` data key. Assert `EnsureEncKey` returns `(wrappedDEK []byte, kekID string, err error)` and that the returned wrapped bytes do not equal any plaintext (round-trip via the test KMS to confirm it unwraps to 32 bytes). Inject a `kms.NewLocalKEK(make([]byte,32))` into the reconciler under test.
- [ ] Run the controller test and confirm it fails to compile (`EnsureEncKey` signature changed; new data keys).
- [ ] Edit `internal/controller/enc_key_secret.go`:
  - Add data-key constants: `encKeyWrappedDataKey = "wrapped-dek"` and `encKeyKEKIDDataKey = "kek-id"`. Keep `encKeyLen = 32` for the DEK.
  - Change the signature to `func EnsureEncKey(ctx context.Context, c client.Client, w kms.Wrapper, ns, templateID string, owner client.Object) (wrappedDEK []byte, kekID string, err error)`.
  - On the NotFound branch: generate `dek := make([]byte, encKeyLen)`, `rand.Read(dek)`, `wrapped, werr := w.Wrap(ctx, dek)`, then zeroize the plaintext DEK (`for i := range dek { dek[i] = 0 }`) immediately. Store the Secret with `Data: map[string][]byte{encKeyWrappedDataKey: wrapped.Ciphertext, encKeyKEKIDDataKey: []byte(wrapped.KEKID)}`. Return `wrapped.Ciphertext, wrapped.KEKID, nil`. Never log the DEK or the wrapped bytes; only the KEK id (non-secret) and the Secret name may appear.
  - On the found branch and the create-race re-read branch: return `secret.Data[encKeyWrappedDataKey]` + `string(secret.Data[encKeyKEKIDDataKey])`; error if `wrapped-dek` is empty (do not regenerate, mirroring the existing logic).
  - Update the doc comment to describe envelope custody: the controller generates the DEK, wraps it with the KMS KEK, zeroizes the plaintext, and persists ONLY the wrapped DEK + KEK id. The plaintext DEK never persists to etcd or disk.
  - Add `"github.com/paperclipinc/mitos/internal/kms"` to imports. `DeleteEncKey` is unchanged (deleting the Secret crypto-shreds the wrapped DEK).
- [ ] Run the controller envtest (`eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run EncKey`) and confirm green.
- [ ] Run `gofmt -l internal/controller/`, both lint invocations on `./internal/controller/...`.
- [ ] `git add internal/controller/enc_key_secret.go internal/controller/enc_key_envtest_test.go`
- [ ] Commit `feat: controller wraps the DEK with the KMS and stores only the wrapped DEK`.

### Task 7: controller delivers the wrapped DEK plus KEK id on the RPCs

**Files:** `internal/controller/sandboxpool_controller.go`, `internal/controller/sandboxclaim_controller.go`, `internal/controller/forkd_client.go`, and a reconciler field for the KMS.

- [ ] Add a `KMS kms.Wrapper` field to `SandboxPoolReconciler` and `SandboxClaimReconciler` (wherever `EnsureEncKey` is called). Document it: "unwrapper/wrapper for envelope-encrypted DEKs; required when any reconciled template is Encrypted".
- [ ] Update `ensureTemplateBuilt` (`sandboxpool_controller.go:261`): change the local `var encKey []byte` plus the `EnsureEncKey` call to capture `wrappedDEK, kekID, keyErr := EnsureEncKey(ctx, r.Client, r.KMS, pool.Namespace, templateID, template)`. Thread `wrappedDEK` and `kekID` into `createSnapshotsOnNodes`.
- [ ] Update `createSnapshotsOnNodes` (`sandboxpool_controller.go:302`): change the `encKey []byte` parameter to `wrappedDEK []byte, kekID string`. The `distribute := len(encKey) == 0 ...` and the mTLS fail-closed guard (`if len(encKey) > 0 && !NodeMTLS ...`) now key off `len(wrappedDEK) > 0`. In the `CreateTemplateRequest`, set `EncryptionKey: wrappedDEK, KekId: kekID`. Keep all comments accurate (the field now carries the WRAPPED DEK).
- [ ] Update the claim/fork path: `sandboxclaim_controller.go:432` calls `EnsureEncKey`; capture `wrappedDEK, kekID`. Thread them into `forkd_client.go` where `ForkRequest` is built (`forkd_client.go:101`): set `EncryptionKey: wrappedDEK, KekId: kekID`. Update the `forkOnNode` (or equivalent) signature to take `wrappedDEK []byte, kekID string`.
- [ ] Update `cmd/controller/main.go`: add a `--kek-file` flag, build `kms.LoadLocalKEKFromFile` when set, and inject it into the reconcilers' `KMS` field. If encryption-enabled templates are reconciled without a KMS, `EnsureEncKey` will fail (w is nil) with a clear error; document the flag as required when any template sets `Encrypted: true`. Add `"github.com/paperclipinc/mitos/internal/kms"` import.
- [ ] Extend the fake forkd in the controller tests (the one recording `CreateTemplateRequest`/`ForkRequest`) to also record `KekId`, and assert the envtest sees a non-empty `EncryptionKey` (wrapped) AND a `KekId` equal to the injected KMS `KEKID()` for an Encrypted template, and empty for a plaintext one. Add this assertion in the same `enc_key_envtest_test.go` or the existing RPC-delivery test.
- [ ] Run the controller envtest suite and confirm green.
- [ ] Run `gofmt -l internal/controller/ cmd/controller/`, both lint invocations on `./internal/controller/...` and `./cmd/controller/...`.
- [ ] `git add internal/controller/sandboxpool_controller.go internal/controller/sandboxclaim_controller.go internal/controller/forkd_client.go cmd/controller/main.go internal/controller/enc_key_envtest_test.go`
- [ ] Commit `feat: controller delivers the wrapped DEK and KEK id over the mTLS RPCs`.

### Task 8: docs, threat model, ROADMAP, full verification, PR

**Files:** `docs/encryption.md`, `docs/threat-model.md`, `ROADMAP.md`.

- [ ] `docs/encryption.md`: add a "KMS/HSM envelope encryption" section (and update the "Open follow-ups" bullet that currently lists it as pending). Describe: the controller generates a DEK, wraps it with the KMS KEK, zeroizes the plaintext, and stores ONLY the wrapped DEK + KEK id in the `<template>-enc-key` Secret (data keys `wrapped-dek`, `kek-id`); the RPC carries the wrapped DEK + KEK id; forkd unwraps via its KMS (`--kek-file` local provider), uses the plaintext DEK for cryptsetup, and zeroizes it immediately. State that the plaintext DEK is NEVER in etcd or on disk: only the wrapped DEK is. State the crypto-shred property: deleting the Secret destroys the only stored wrapped DEK, and destroying/rotating the KEK crypto-shreds every DEK it wrapped. Update the trust-boundary text: etcd now holds only the WRAPPED DEK, so the etcd-at-rest-encryption assumption is downgraded to defense-in-depth (an etcd exfiltration without the KEK cannot unwrap). State what is PROVEN in CI (the `internal/kms` round-trip/tamper/KEK-mismatch unit tests; the envtest proving the Secret stores the wrapped DEK + KEK id and never a raw key, and that the RPC carries them; the existing LUKS KVM proof is unchanged) vs OPEN (cloud KMS providers AWS/GCP/Vault as interface-only follow-up; KEK rotation/re-wrap; per-workspace scope #21).
- [ ] `docs/threat-model.md`: update the "Encryption at rest + crypto-shredding (#31)" row (line ~370). Change residual (1): etcd now holds only the WRAPPED DEK; the KEK custody is the `internal/kms` Wrapper (local AES-256-GCM from a Secret-mounted KEK file in dev/CI; cloud KMS/HSM is the documented follow-up where the KEK never leaves the HSM). The in-memory-DEK window (residual 3) is unchanged: the plaintext DEK is necessarily in forkd memory during an open and is zeroized immediately after; full HSM custody narrows but cannot eliminate this. Add: the controller no longer holds the plaintext DEK after `EnsureEncKey` returns (it zeroizes immediately post-wrap). Keep the row honest about cloud KMS being interface-only here.
- [ ] `ROADMAP.md`: update the #31 follow-up list (line ~388) to mark KMS/HSM envelope wrapping as DONE for the local provider with cloud KMS as interface-shaped follow-up; keep KEK rotation and per-workspace scope open.
- [ ] Grep that no secret is logged: `grep -rn "plaintextDEK\|dek\b\|kek)" internal/kms internal/fork internal/controller internal/daemon cmd | grep -i "log\|Printf\|Errorf"` and manually confirm every match formats only a KEK id, a scope, or a count, never DEK/KEK bytes. Confirm `internal/kms/local.go` never formats `kek` or `plaintextDEK` into an error.
- [ ] Full verification:
  - `go build ./...` and `GOOS=linux GOARCH=amd64 go build ./...` and `GOOS=linux GOARCH=amd64 go build ./guest/agent/`.
  - `go vet ./...`.
  - `gofmt -l .` returns empty.
  - `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`, both clean.
  - `make test-unit`, the controller envtest (`eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`), `make test-python`.
  - Dash grep: `grep -rn $'—\|–' internal/kms docs/encryption.md docs/threat-model.md ROADMAP.md proto/forkd.proto cmd` returns empty.
  - Confirm `internal/forkdpb` regen is committed and `make proto` is a no-op diff.
- [ ] `git add docs/encryption.md docs/threat-model.md ROADMAP.md`
- [ ] Commit `docs: document KMS envelope key custody and update the threat model`.
- [ ] Push `feat/rename-to-mitos` (or a `feat/kms-envelope-custody` branch off it per the branch-naming rule). Open a PR titled `KMS envelope key custody for encryption at rest` with a body that: references issue #31's KMS/HSM follow-up; states the local AES-256-GCM provider is the CI-testable default and cloud KMS (AWS/GCP/Vault) is interface-only follow-up; lists what is proven in CI; ends with the PR trailer line. Watch all six required checks to green before merge.

**Out of scope (follow-ups, interface only):**

- Cloud KMS providers: AWS KMS (`kms:Encrypt`/`kms:Decrypt`), GCP KMS, and HashiCorp Vault Transit. Each is a new file in `internal/kms` implementing `Wrapper`; the `Wrap`/`Unwrap` context bounds the remote call; the KEK never leaves the HSM. No cloud SDK is added in this PR.
- KEK rotation and DEK re-wrap: rotate the KEK and re-wrap every stored wrapped DEK (the `KEKID` mismatch in `Unwrap` is the rotation-detection hook this plan installs).
- Per-workspace scope (Workspace #21): make the scope a workspace so one KEK rotation crypto-shreds an entire workspace.
- Sealing the KEK to a node TPM; protecting the in-memory plaintext-DEK window during an open (the DEK is necessarily in memory to serve I/O; a remote HSM that performs the LUKS unlock would be required to eliminate it, which dm-crypt does not support today).
