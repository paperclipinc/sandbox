package daemon

import (
	"strings"
	"testing"
)

func TestValidateSandboxID(t *testing.T) {
	valid := []string{
		"a",
		"sb-1234",
		"vm_7",
		"A0",
		"py",
		"0leading-digit",
		strings.Repeat("a", 64),
	}
	for _, id := range valid {
		if err := validateSandboxID(id); err != nil {
			t.Errorf("validateSandboxID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",
		".",
		"..",
		"../x",
		"a/b",
		"a.b",
		"-leading-hyphen",
		"_leading-underscore",
		strings.Repeat("a", 65),
		"unicode-é",
		"emoji-\U0001f600",
		"space here",
		"null\x00byte",
		"/abs",
		"a\\b",
	}
	for _, id := range invalid {
		if err := validateSandboxID(id); err == nil {
			t.Errorf("validateSandboxID(%q) = nil, want error", id)
		}
	}
}
