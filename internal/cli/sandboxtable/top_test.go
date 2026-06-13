package sandboxtable

import (
	"strings"
	"testing"

	"github.com/paperclipinc/mitos/internal/metering"
)

func TestFormatTopRendersCoWAwareRows(t *testing.T) {
	rows := []TopRow{
		{
			Name:  "alpha",
			Node:  "node-1",
			Found: true,
			Datum: metering.SandboxMetering{
				MemoryUnique: 12 * 1024 * 1024,  // 12 MiB private-dirty
				MemoryShared: 256 * 1024 * 1024, // 256 MiB template shared
				DiskUnique:   2 * 1024 * 1024,
			},
		},
		{
			// No metering datum: every metered cell must be a dash.
			Name:  "beta",
			Node:  "node-2",
			Found: false,
		},
	}
	out := FormatTop(rows)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 rows, got %d:\n%s", len(lines), out)
	}
	for _, col := range []string{"NAME", "NODE", "UNIQUE-MEM", "SHARED-MEM", "UNIQUE-DISK"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header missing column %q: %q", col, lines[0])
		}
	}
	// Honest labeling: the column header is UNIQUE-MEM / SHARED-MEM, never a raw
	// "MEMORY" that would imply memory.current.
	if strings.Contains(lines[0], "MEMORY ") {
		t.Errorf("top must not label a raw MEMORY column: %q", lines[0])
	}

	alpha := strings.Fields(lines[1])
	if alpha[0] != "alpha" || alpha[1] != "node-1" {
		t.Errorf("alpha row name/node = %q,%q", alpha[0], alpha[1])
	}
	// 12 MiB unique, 256 MiB shared rendered in IEC units.
	if !strings.Contains(lines[1], "12.0 MiB") {
		t.Errorf("alpha unique-mem should be 12.0 MiB: %q", lines[1])
	}
	if !strings.Contains(lines[1], "256.0 MiB") {
		t.Errorf("alpha shared-mem should be 256.0 MiB: %q", lines[1])
	}
}

func TestFormatTopMissingDatumIsDashNeverZero(t *testing.T) {
	rows := []TopRow{{Name: "beta", Node: "node-2", Found: false}}
	out := FormatTop(rows)
	row := strings.Fields(strings.Split(strings.TrimRight(out, "\n"), "\n")[1])
	// beta node-2 - - -  : the three metered cells are all dashes, never "0 B".
	if len(row) != 5 {
		t.Fatalf("expected 5 fields, got %v", row)
	}
	for i := 2; i < 5; i++ {
		if row[i] != "-" {
			t.Errorf("metered cell %d should be a dash for a missing datum, got %q", i, row[i])
		}
	}
	if strings.Contains(out, "0 B") {
		t.Errorf("a missing datum must never render as 0 B: %q", out)
	}
}

func TestFormatTopEmpty(t *testing.T) {
	if out := FormatTop(nil); !strings.Contains(out, "No sandboxes found") {
		t.Errorf("empty top should report no sandboxes, got %q", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{12 * 1024 * 1024, "12.0 MiB"},
		{0, "0 B"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
