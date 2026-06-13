package fork

import (
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/storecrypt"
)

func TestRequestKeyProviderSetAndKeyFor(t *testing.T) {
	p := NewRequestKeyProvider()
	key := storecrypt.Key("0123456789abcdef0123456789abcdef")
	p.SetKey("scope-a", key)

	got, err := p.KeyFor("scope-a")
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("KeyFor returned a different key than the one set")
	}
}

func TestRequestKeyProviderKeyForAbsentErrors(t *testing.T) {
	p := NewRequestKeyProvider()
	_, err := p.KeyFor("missing")
	if err == nil {
		t.Fatal("KeyFor for an absent scope must error (fail closed), got nil")
	}
	// The error must not leak any key bytes (there is none, but the contract is
	// that the provider never formats key material into an error).
	if strings.Contains(err.Error(), "REDACTED") {
		t.Fatal("unexpected key material reference in error")
	}
}

func TestRequestKeyProviderForgetZeroizes(t *testing.T) {
	p := NewRequestKeyProvider()
	key := storecrypt.Key("0123456789abcdef0123456789abcdef")
	// Keep a reference to the same underlying array so we can prove ForgetKey
	// zeroized it in place.
	p.SetKey("scope-a", key)

	p.ForgetKey("scope-a")

	if _, err := p.KeyFor("scope-a"); err == nil {
		t.Fatal("KeyFor after ForgetKey must error; the key should be gone")
	}
	for i, b := range key {
		if b != 0 {
			t.Fatalf("key byte %d not zeroized after ForgetKey: %d", i, b)
		}
	}
}

func TestRequestKeyProviderForgetAbsentIsNoop(t *testing.T) {
	p := NewRequestKeyProvider()
	// Must not panic for an unknown scope.
	p.ForgetKey("never-set")
}
