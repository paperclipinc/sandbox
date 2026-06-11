package storecrypt

import (
	"bytes"
	"fmt"
	"testing"
)

func TestNewKeyIs32RandomBytes(t *testing.T) {
	k1, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("key length = %d, want 32", len(k1))
	}
	k2, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("two NewKey results are identical; not random")
	}
	if allZero(k1) {
		t.Fatal("fresh key is all zero; not random")
	}
}

func TestZeroizeClearsKey(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	k.Zeroize()
	if !allZero(k) {
		t.Fatal("Zeroize did not clear the key bytes")
	}
	if len(k) != 32 {
		t.Fatalf("Zeroize changed length to %d", len(k))
	}
}

// TestKeyNeverLeaksInTextForms asserts every text rendering of a Key is the
// fixed redacted placeholder and never contains the raw bytes.
func TestKeyNeverLeaksInTextForms(t *testing.T) {
	// Use a key with recognizable, printable-ascii bytes so a leak would be
	// visible in a string form.
	raw := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ012345")
	k := Key(raw)

	// %v and %s both route through Stringer; build the verbs indirectly so the
	// linter does not rewrite the %s case it is precisely meant to exercise.
	forms := []string{
		k.String(),
		sprintfVerb("v", k),
		sprintfVerb("s", k),
	}
	mt, err := k.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	forms = append(forms, string(mt))

	for _, f := range forms {
		if f != "[REDACTED key]" {
			t.Errorf("text form is not the redacted placeholder: %q", f)
		}
		if bytes.Contains([]byte(f), raw) {
			t.Errorf("text form leaked the raw key bytes: %q", f)
		}
	}
}

// sprintfVerb formats k with the given verb, indirecting the format string so
// the gosimple linter does not rewrite a literal "%s" into a String() call (the
// %s path is exactly what this test must exercise).
func sprintfVerb(verb string, k Key) string {
	return fmt.Sprintf("%"+verb, k)
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
