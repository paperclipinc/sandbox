package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTemplateManagerRejectsTraversalIDs asserts the defense-in-depth
// validateVMID barrier at the TemplateManager entry points: an id that could
// introduce a path separator or a traversal segment is refused before it is
// joined into a filesystem path, so a path-injection cannot escape the data
// dir even if the gRPC-boundary guard is ever bypassed.
func TestTemplateManagerRejectsTraversalIDs(t *testing.T) {
	dataDir := t.TempDir()
	tm := &TemplateManager{dataDir: dataDir}

	bad := []string{"../escape", "a/b", "..", "../../etc", "with space", "", "/abs"}

	for _, id := range bad {
		if _, err := tm.CreateTemplate(id, VMConfig{}, nil); err == nil {
			t.Errorf("CreateTemplate(%q) returned nil error; expected validation rejection", id)
		}
		if err := tm.DeleteTemplate(id); err == nil {
			t.Errorf("DeleteTemplate(%q) returned nil error; expected validation rejection", id)
		}
		if tm.HasTemplate(id) {
			t.Errorf("HasTemplate(%q) returned true for an invalid id", id)
		}
	}

	// A delete of an invalid id must not have removed anything from the data dir
	// (the barrier returns before os.RemoveAll runs).
	sentinel := filepath.Join(dataDir, "templates", "keep")
	if err := os.MkdirAll(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tm.DeleteTemplate("../templates"); err == nil {
		t.Error("DeleteTemplate(../templates) should be rejected")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("a rejected DeleteTemplate must not remove anything: %v", err)
	}

	// A valid id passes the barrier (HasTemplate returns false only because no
	// such template exists, not because the id was rejected).
	if tm.HasTemplate("valid-id_1") {
		t.Error("HasTemplate(valid-id_1) should be false (no such template), not error out")
	}
}
