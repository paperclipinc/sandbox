package sandboxtable

import (
	"fmt"
	"sort"

	"github.com/paperclipinc/mitos/internal/metering"
)

// TopRow is one sandbox's line in the top view. Name is the claim/fork object
// name the operator knows; Node is the forkd node it landed on. Datum is the
// CoW-aware metering sample for the sandbox, and Found reports whether any
// metering source actually returned a row for it. When Found is false EVERY
// metered cell renders as a dash: top never invents a zero for a sandbox the
// metering source did not account for (the no-unverified-numbers rule).
type TopRow struct {
	Name  string
	Node  string
	Datum metering.SandboxMetering
	Found bool
}

// FormatTop renders per-sandbox CoW-aware metering. Columns:
//
//	NAME           the claim/fork object name
//	NODE           the forkd node
//	UNIQUE-MEM     marginal unique (private-dirty) memory this sandbox alone owns
//	SHARED-MEM     shared-once attribution: template memory shared across forks
//	UNIQUE-DISK    backing storage this sandbox alone owns
//
// The memory columns are deliberately NOT raw memory.current: UNIQUE-MEM is the
// private-dirty pages a fork actually adds, and SHARED-MEM is the template page
// set the fork maps copy-on-write (counted once per template at the node level,
// see internal/metering). A sandbox with no metering datum shows a dash in every
// metered cell, never a zero or a fabricated value. Byte counts render in IEC
// units. now is unused; the signature matches the other formatters for symmetry.
func FormatTop(rows []TopRow) string {
	if len(rows) == 0 {
		return "No sandboxes found.\n"
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	header := []string{"NAME", "NODE", "UNIQUE-MEM", "SHARED-MEM", "UNIQUE-DISK"}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		uniqueMem, sharedMem, uniqueDisk := dash, dash, dash
		if r.Found {
			uniqueMem = humanBytes(r.Datum.MemoryUnique)
			sharedMem = humanBytes(r.Datum.MemoryShared)
			uniqueDisk = humanBytes(r.Datum.DiskUnique)
		}
		out = append(out, []string{
			r.Name,
			orDash(r.Node),
			uniqueMem,
			sharedMem,
			uniqueDisk,
		})
	}
	return renderTable(header, out)
}

// humanBytes renders a byte count in IEC units (KiB, MiB, GiB) with one decimal
// place above the kibibyte boundary, or a plain byte count below it. It is only
// ever called for a present datum; a missing datum renders as a dash upstream,
// so a literal "0 B" here means a real zero-byte sample, never an absent one.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
