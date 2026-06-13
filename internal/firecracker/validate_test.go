package firecracker

import "testing"

func TestValidateVMID(t *testing.T) {
	accept := []string{"sb-1", "Abc_9", "a", "0", "sandbox", "VM-with_underscores-1"}
	for _, id := range accept {
		if err := validateVMID(id); err != nil {
			t.Errorf("validateVMID(%q) = %v, want nil", id, err)
		}
	}

	reject := []string{
		"",              // empty
		"..",            // bare traversal
		"../x",          // traversal prefix
		"a/b",           // path separator
		"/abs",          // absolute path
		"a.b",           // dot is not allowed
		"-leading",      // must start with letter or digit
		repeat("a", 65), // 65 chars exceeds the 64-char ceiling
	}
	for _, id := range reject {
		if err := validateVMID(id); err == nil {
			t.Errorf("validateVMID(%q) = nil, want error", id)
		}
	}
}

func TestGuardExportPath(t *testing.T) {
	root := "/var/lib/mitos"
	cases := []struct {
		path string
		ok   bool
	}{
		{"/var/lib/mitos", true},
		{"/var/lib/mitos/templates/sb-1/snapshot/mem", true},
		{"/var/lib/mitos/../etc/passwd", false},
		{"/etc/passwd", false},
		{"/var/lib/mitos-evil/mem", false},
	}
	for _, tc := range cases {
		err := guardExportPath(tc.path, root)
		if tc.ok && err != nil {
			t.Errorf("guardExportPath(%q, %q) = %v, want nil", tc.path, root, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("guardExportPath(%q, %q) = nil, want error", tc.path, root)
		}
	}
	// An empty data dir disables the check.
	if err := guardExportPath("/anywhere", ""); err != nil {
		t.Errorf("guardExportPath with empty dataDir = %v, want nil", err)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
