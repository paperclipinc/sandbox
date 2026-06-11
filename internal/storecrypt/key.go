// Package storecrypt manages per-scope LUKS containers that hold template
// snapshots encrypted at rest. A scope (e.g. a template id) gets its own
// LUKS2 container backed by a sparse image file; the container is opened and
// mounted at the scope's data directory so the snapshot read/write paths run
// unchanged on top of a decrypted dm-crypt device. Tearing a scope down
// crypto-shreds the container: the LUKS keyslots are erased and the image is
// removed, rendering the ciphertext unrecoverable.
//
// The encryption key is a secret VALUE. It is NEVER logged, NEVER placed in a
// process argv, and NEVER embedded in an error message. The key reaches
// cryptsetup only through the child process stdin (cryptsetup --key-file -),
// so it never appears on a command line visible to other users via /proc.
package storecrypt

import (
	"crypto/rand"
	"fmt"
)

// keyLen is the LUKS key length in bytes (256-bit).
const keyLen = 32

// Key is a 256-bit symmetric key for a LUKS container. It is a secret value:
// it must never be logged, formatted into an error, or passed in argv. Its
// String and MarshalText implementations deliberately return a fixed redacted
// placeholder so an accidental %v, %s, log, or JSON/text marshal can never leak
// the bytes.
type Key []byte

// NewKey returns a fresh random 256-bit key from crypto/rand.
func NewKey() (Key, error) {
	k := make(Key, keyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return k, nil
}

// Zeroize overwrites the key bytes with zeros so the secret does not linger in
// memory after the key is no longer needed. The slice keeps its length; only
// the contents are cleared.
func (k Key) Zeroize() {
	for i := range k {
		k[i] = 0
	}
}

// redacted is the fixed placeholder returned wherever a Key would otherwise be
// rendered as text. It never contains key material.
const redacted = "[REDACTED key]"

// String returns a fixed redacted placeholder, NEVER the key bytes, so a Key
// caught by %v/%s or a logger does not leak. Key intentionally satisfies
// fmt.Stringer with this redaction rather than exposing its bytes.
func (k Key) String() string { return redacted }

// MarshalText returns the redacted placeholder so a Key serialized as text
// (JSON, YAML via text marshaling) never carries the secret bytes.
func (k Key) MarshalText() ([]byte, error) { return []byte(redacted), nil }
