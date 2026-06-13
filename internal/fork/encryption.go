package fork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/paperclipinc/mitos/internal/storecrypt"
)

// containerManager is the narrow seam the engine uses for per-scope encrypted
// containers. The real *storecrypt.Manager satisfies it; engine tests inject a
// fake that records calls and uses a plain directory as the "mount" so the
// snapshot write/read logic runs without dm-crypt. Encryption is below the page
// cache: once a container is open and mounted, reads and writes (including the
// mem mmap CoW restore path) see decrypted bytes, so CoW page sharing across
// forks is preserved exactly as in the plaintext case.
type containerManager interface {
	Create(ctx context.Context, scopeID string, key storecrypt.Key, sizeBytes int64, mountPoint string) error
	Open(ctx context.Context, scopeID string, key storecrypt.Key, mountPoint string) error
	Close(ctx context.Context, scopeID, mountPoint string) error
	Shred(ctx context.Context, scopeID, mountPoint string) error
}

// KeyProvider supplies the encryption key for a scope (a template id). PR1 ships
// an in-memory provider that generates and caches a key per scope in node
// memory; PR2 swaps in a provider backed by a Kubernetes Secret or a KMS so the
// key custody moves off the node. KeyFor is called on the template build path
// and on the (cold) container-open path, never per fork once a container is
// already open. ForgetKey destroys the in-memory key for a scope as part of
// crypto-shred: after it returns the provider holds no copy of the key.
type KeyProvider interface {
	KeyFor(scopeID string) (storecrypt.Key, error)
	ForgetKey(scopeID string)
}

// InMemoryKeyProvider generates a random 256-bit key per scope and caches it in
// process memory. PR1 KEY-CUSTODY LIMITATION: keys live only in node memory and
// are lost on restart (an existing encrypted template can then no longer be
// opened) and are NOT escrowed anywhere. This is the PR1 placeholder; PR2
// replaces it with a Secret/KMS-backed provider. It is safe for concurrent use.
type InMemoryKeyProvider struct {
	mu   sync.Mutex
	keys map[string]storecrypt.Key
}

// NewInMemoryKeyProvider returns an empty in-memory key provider.
func NewInMemoryKeyProvider() *InMemoryKeyProvider {
	return &InMemoryKeyProvider{keys: make(map[string]storecrypt.Key)}
}

// KeyFor returns the cached key for scopeID, generating and caching a fresh one
// on first use. The key value is never logged.
func (p *InMemoryKeyProvider) KeyFor(scopeID string) (storecrypt.Key, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[scopeID]; ok {
		return k, nil
	}
	k, err := storecrypt.NewKey()
	if err != nil {
		return nil, fmt.Errorf("generate key for scope %s: %w", scopeID, err)
	}
	p.keys[scopeID] = k
	return k, nil
}

// ForgetKey destroys the in-memory key for scopeID: it zeroizes the key bytes
// (clearing the underlying array shared with any caller copy) and deletes the
// map entry. After it returns a subsequent KeyFor(scopeID) generates a fresh
// key. It is called from crypto-shred so the key does not linger in node memory
// after the container is erased. A no-op for a scope with no cached key.
func (p *InMemoryKeyProvider) ForgetKey(scopeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[scopeID]; ok {
		k.Zeroize()
		delete(p.keys, scopeID)
	}
}

// hasKey reports whether the provider currently holds a cached key for scopeID.
// It is test-only support for asserting that crypto-shred forgot the key.
func (p *InMemoryKeyProvider) hasKey(scopeID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.keys[scopeID]
	return ok
}

// RequestKeyProvider holds per-scope keys delivered by the controller over the
// mTLS RPC (PR2 key custody). Unlike InMemoryKeyProvider it never generates a
// key: the daemon stashes the request-supplied key with SetKey before invoking
// the engine and ForgetKey after. KeyFor on a scope with no stashed key returns
// an error so encryption FAILS CLOSED (the engine must not silently run
// unencrypted). The key is a secret value: it is never logged, never formatted
// into an error, and the type exposes no String() that could leak it. Safe for
// concurrent use.
type RequestKeyProvider struct {
	mu   sync.Mutex
	keys map[string]storecrypt.Key
}

// NewRequestKeyProvider returns an empty request-scoped key provider.
func NewRequestKeyProvider() *RequestKeyProvider {
	return &RequestKeyProvider{keys: make(map[string]storecrypt.Key)}
}

// SetKey stashes the key the controller delivered for scopeID for the duration
// of the operation. The daemon calls it before invoking the engine; the key
// value is never logged.
func (p *RequestKeyProvider) SetKey(scopeID string, key storecrypt.Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[scopeID] = key
}

// KeyFor returns the stashed key for scopeID. It returns an error when no key
// is present so the engine fails closed rather than running unencrypted. The
// error names only the scope, never any key material.
func (p *RequestKeyProvider) KeyFor(scopeID string) (storecrypt.Key, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[scopeID]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("no encryption key available for scope %s: the request must carry the key over mTLS", scopeID)
}

// ForgetKey zeroizes the stashed key bytes for scopeID and deletes the entry.
// After it returns the provider holds no copy of the key. A no-op for a scope
// with no stashed key.
func (p *RequestKeyProvider) ForgetKey(scopeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[scopeID]; ok {
		k.Zeroize()
		delete(p.keys, scopeID)
	}
}

// encryptionEnabled reports whether at-rest encryption is wired: the flag is set
// and both a container manager and a key provider are present.
func (e *Engine) encryptionEnabled() bool {
	return e.enableEncryption && e.crypt != nil && e.keyProvider != nil
}

// encryptedMarker is the file written inside a template dir to record that the
// template's snapshot lives in an encrypted container, so Fork knows to open
// the container before restoring. It sits inside the container mount, so it is
// itself encrypted at rest.
const encryptedMarker = ".encrypted"

// templateEncrypted reports whether a template was built into an encrypted
// container, by checking for the marker file in its data dir. A missing marker
// (or encryption disabled) means the plaintext path.
func (e *Engine) templateEncrypted(id string) bool {
	if !e.encryptionEnabled() {
		return false
	}
	_, err := os.Stat(filepath.Join(templateDir(e.dataDir, id), encryptedMarker))
	return err == nil
}

// templateFootprintSize estimates the bytes an encrypted container must hold for
// a template and adds headroom, with a floor. The snapshot mem+vmstate+rootfs
// and any seed volumes are written into the container, so the container is sized
// to their sum plus headroom; a generous floor avoids a too-small filesystem for
// small templates. This runs before the snapshot exists (the container must be
// created first), so it is an estimate derived from the rootfs size and the
// volume specs.
func templateFootprintSize(rootfsPath string, volumes []volSize) int64 {
	const (
		floor    = int64(256) << 20 // 256 MiB minimum container
		headroom = int64(128) << 20 // slack for filesystem overhead + vmstate + mem
	)
	var total int64
	if fi, err := os.Stat(rootfsPath); err == nil {
		total += fi.Size()
	}
	for _, v := range volumes {
		total += v.sizeBytes
	}
	total += headroom
	if total < floor {
		return floor
	}
	return total
}

// volSize is the minimal volume sizing view the footprint estimate needs.
type volSize struct {
	sizeBytes int64
}

// createTemplateContainer creates and mounts an encrypted container at the
// template dir, sized to the template footprint, and writes the encrypted
// marker. It is called from CreateTemplate BEFORE the snapshot is built so the
// build writes its files inside the container. Returns nil (no-op) when
// encryption is disabled.
func (e *Engine) createTemplateContainer(id, rootfsPath string, volumes []volSize) error {
	if !e.encryptionEnabled() {
		return nil
	}
	key, err := e.keyProvider.KeyFor(id)
	if err != nil {
		return fmt.Errorf("key for template %s: %w", id, err)
	}
	dir := templateDir(e.dataDir, id)
	size := templateFootprintSize(rootfsPath, volumes)
	if err := e.crypt.Create(context.Background(), id, key, size, dir); err != nil {
		return fmt.Errorf("create encrypted container for template %s: %w", id, err)
	}
	e.markTemplateOpen(id)
	if err := os.WriteFile(filepath.Join(dir, encryptedMarker), nil, 0o600); err != nil {
		return fmt.Errorf("write encrypted marker for template %s: %w", id, err)
	}
	return nil
}

// markTemplateOpen records that a template's container is open and mounted so
// the keep-open logic does not re-open it on every fork.
func (e *Engine) markTemplateOpen(id string) {
	e.mu.Lock()
	if e.encOpen == nil {
		e.encOpen = make(map[string]struct{})
	}
	e.encOpen[id] = struct{}{}
	e.mu.Unlock()
}

// ensureTemplateOpen opens and mounts an encrypted template's container if it is
// not already open (e.g. after a restart, or forking a template this process
// did not build). It is a no-op for plaintext templates and when encryption is
// disabled. The container is kept open across forks: it is opened once and only
// closed/shredded at template delete, so the hot fork path does not pay an
// open+mount per fork. Encryption is below the page cache, so the mem mmap CoW
// restore that follows reads decrypted pages and preserves cross-fork sharing.
func (e *Engine) ensureTemplateOpen(id string) error {
	if !e.templateEncrypted(id) {
		return nil
	}
	// Fast path: already open. A cheap RLock-guarded map check, taken on the hot
	// fork path once the first fork has opened the container.
	e.mu.RLock()
	_, open := e.encOpen[id]
	e.mu.RUnlock()
	if open {
		return nil
	}
	// Slow path: serialize the open per template so concurrent forks of the same
	// template (after a restart, with an empty encOpen map) do not both call
	// crypt.Open (the second luksOpen would fail: device already exists). The
	// per-template lock lets the first opener run while the rest wait, then they
	// re-check and find it open.
	mu := e.openLock(id)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under the per-template lock: a concurrent caller may have opened
	// the container while we waited.
	e.mu.RLock()
	_, open = e.encOpen[id]
	e.mu.RUnlock()
	if open {
		return nil
	}

	key, err := e.keyProvider.KeyFor(id)
	if err != nil {
		return fmt.Errorf("key for template %s: %w", id, err)
	}
	if err := e.crypt.Open(context.Background(), id, key, templateDir(e.dataDir, id)); err != nil {
		return fmt.Errorf("open encrypted container for template %s: %w", id, err)
	}
	e.markTemplateOpen(id)
	return nil
}

// openLock returns the per-template mutex that serializes ensureTemplateOpen for
// id, creating it on first use. Only one *sync.Mutex ever exists per id.
func (e *Engine) openLock(id string) *sync.Mutex {
	mu, _ := e.encOpenLocks.LoadOrStore(id, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// shredTemplateContainer crypto-shreds an encrypted template's container at
// template delete/GC: best-effort umount + luksClose, then erase the keyslots
// and remove the image. Individual fork Terminate never calls this (sibling
// forks may share the open container); only template teardown does. No-op when
// encryption is disabled or the template is not encrypted.
func (e *Engine) shredTemplateContainer(id string) error {
	if !e.encryptionEnabled() {
		return nil
	}
	if !e.templateEncrypted(id) {
		// Container not open via this process and no marker on disk: nothing to
		// shred, but stay idempotent in case the marker lives inside an
		// already-unmounted container. Shred tolerates a missing container.
		if err := e.crypt.Shred(context.Background(), id, templateDir(e.dataDir, id)); err != nil {
			return err
		}
		// Destroy the in-memory key AFTER the keyslots are erased so the secret
		// does not linger in node memory once the ciphertext is unrecoverable.
		e.keyProvider.ForgetKey(id)
		e.forgetTemplateOpen(id)
		return nil
	}
	if err := e.crypt.Shred(context.Background(), id, templateDir(e.dataDir, id)); err != nil {
		return fmt.Errorf("shred encrypted container for template %s: %w", id, err)
	}
	// Destroy the in-memory key AFTER the keyslots are erased so the secret does
	// not linger in node memory once the ciphertext is unrecoverable.
	e.keyProvider.ForgetKey(id)
	e.forgetTemplateOpen(id)
	return nil
}

// forgetTemplateOpen drops the keep-open record for a template after its
// container is closed or shredded.
func (e *Engine) forgetTemplateOpen(id string) {
	e.mu.Lock()
	delete(e.encOpen, id)
	e.mu.Unlock()
}
