// Package sandboxtable renders SandboxClaim and SandboxFork lists as aligned
// kubectl-style tables. The formatting functions are pure (no cluster access),
// so they carry the kubectl-sandbox plugin's test coverage; the live listing in
// cmd/kubectl-sandbox is a thin wrapper around them.
package sandboxtable

import (
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
)

// dash is the placeholder for an empty cell (missing node, endpoint, or source).
const dash = "-"

// formatAge renders a duration the way kubectl does: the single largest unit,
// seconds under a minute, then minutes, hours, and days. Negative or zero ages
// render as "0s".
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// orDash returns s, or "-" when s is empty.
func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

// renderTable lays out header + rows as a left-aligned, space-padded table with
// a trailing newline. Each column is widened to its longest cell.
func renderTable(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i == len(cells)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		b.WriteByte('\n')
	}
	writeRow(header)
	for _, row := range rows {
		writeRow(row)
	}
	return b.String()
}

// FormatClaims renders SandboxClaims as a table with columns NAME, POOL, PHASE,
// NODE, ENDPOINT, AGE. now anchors the age column. An empty list returns a
// "No sandboxes found" message.
func FormatClaims(claims []v1alpha1.SandboxClaim, now time.Time) string {
	if len(claims) == 0 {
		return "No sandboxes found.\n"
	}
	header := []string{"NAME", "POOL", "PHASE", "NODE", "ENDPOINT", "AGE"}
	rows := make([][]string, 0, len(claims))
	for i := range claims {
		c := &claims[i]
		rows = append(rows, []string{
			c.Name,
			orDash(c.Spec.PoolRef.Name),
			orDash(string(c.Status.Phase)),
			orDash(c.Status.Node),
			orDash(c.Status.Endpoint),
			formatAge(now.Sub(c.CreationTimestamp.Time)),
		})
	}
	return renderTable(header, rows)
}

// FormatForks renders SandboxForks as a table with columns NAME, SOURCE, READY,
// AGE. READY is "<readyForks>/<totalForks>", falling back to the spec replica
// count when the status total is unset. An empty list returns a
// "No forks found" message.
func FormatForks(forks []v1alpha1.SandboxFork, now time.Time) string {
	if len(forks) == 0 {
		return "No forks found.\n"
	}
	header := []string{"NAME", "SOURCE", "READY", "AGE"}
	rows := make([][]string, 0, len(forks))
	for i := range forks {
		f := &forks[i]
		total := f.Status.TotalForks
		if total == 0 {
			total = f.Spec.Replicas
		}
		rows = append(rows, []string{
			f.Name,
			orDash(f.Spec.SourceRef.Name),
			fmt.Sprintf("%d/%d", f.Status.ReadyForks, total),
			formatAge(now.Sub(f.CreationTimestamp.Time)),
		})
	}
	return renderTable(header, rows)
}
