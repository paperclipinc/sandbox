package guestenv

import (
	"slices"
	"testing"
)

func TestMergePrecedence(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/root", "LANG=C"}
	configured := map[string]string{"HOME": "/workspace", "API_KEY": "k1"}
	request := map[string]string{"API_KEY": "k2", "EXTRA": "e"}

	got := Merge(base, configured, request)

	want := map[string]string{
		"PATH":    "/bin",       // base survives
		"LANG":    "C",          // base survives
		"HOME":    "/workspace", // configured overrides base
		"API_KEY": "k2",         // request overrides configured
		"EXTRA":   "e",          // request adds
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vars %v, want %d", len(got), got, len(want))
	}
	for k, v := range want {
		if !slices.Contains(got, k+"="+v) {
			t.Errorf("missing %s=%s in %v", k, v, got)
		}
	}
}

func TestMergeNilMaps(t *testing.T) {
	got := Merge([]string{"A=1"}, nil, nil)
	if len(got) != 1 || got[0] != "A=1" {
		t.Fatalf("got %v", got)
	}
}

func TestMergeBaseEmptyValueSurvives(t *testing.T) {
	got := Merge([]string{"KEY="}, nil, nil)
	if !slices.Contains(got, "KEY=") {
		t.Fatalf("KEY= (empty value) dropped: got %v", got)
	}
}

func TestMergeBaseNoEqualsPassesThroughVerbatim(t *testing.T) {
	got := Merge([]string{"NOEQUALS", "A=1"}, nil, nil)
	if !slices.Contains(got, "NOEQUALS") {
		t.Fatalf("NOEQUALS dropped: got %v", got)
	}
	if !slices.Contains(got, "A=1") {
		t.Fatalf("A=1 missing: got %v", got)
	}
}
