package fork

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/paperclipinc/sandbox/internal/metering"
)

// defaultMemoryReserveBytes is the OS/forkd reserve withheld from the reported
// node memory budget when no explicit reserve is configured: 2 GiB. The
// controller's available-memory math leans on MemoryTotal being the schedulable
// budget, not the raw machine total, so a reserve keeps the node from being
// packed into OOM.
const defaultMemoryReserveBytes int64 = 2 * 1024 * 1024 * 1024

// defaultForkUniqueFloorBytes is the per-fork unique footprint estimate used
// when a template has no live forks to average over (8 MiB). It keeps a
// cold-only template's marginal cost non-zero so the scheduler does not treat a
// never-forked template as free.
const defaultForkUniqueFloorBytes int64 = 8 * 1024 * 1024

// TemplateEstimate is the engine-side per-template capacity estimate carried in
// Capacity. It mirrors proto TemplateCapacity. SharedOnceBytes is the CoW
// shared set a cold start of this template pays once; AvgForkUniqueBytes is the
// mean per-fork unique footprint every fork pays (a floor when ForkCount is 0).
type TemplateEstimate struct {
	TemplateID         string
	SnapshotDigest     string
	SharedOnceBytes    int64
	AvgForkUniqueBytes int64
	ForkCount          int32
}

// hostMemTotalBytes parses MemTotal (in kB) from the /proc/meminfo contents the
// reader returns and converts it to bytes. The reader is injectable so tests
// feed a canned file and so non-linux/dev paths can supply a failing reader.
func hostMemTotalBytes(reader func() (string, error)) (int64, error) {
	content, err := reader()
	if err != nil {
		return 0, fmt.Errorf("read meminfo: %w", err)
	}
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "MemTotal:"))
		if len(fields) < 1 {
			return 0, fmt.Errorf("meminfo MemTotal malformed: %q", line)
		}
		kb, perr := strconv.ParseInt(fields[0], 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("parse MemTotal %q: %w", fields[0], perr)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("meminfo has no MemTotal line")
}

// procMeminfoReader reads /proc/meminfo. It is the production reader passed to
// hostMemTotalBytes; on non-linux hosts the read fails and the caller treats
// MemTotal as unknown (0).
func procMeminfoReader() (string, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// templateEstimateFloor is the estimate for a template with no live forks: a
// zero shared set and the per-fork unique floor.
func templateEstimateFloor() TemplateEstimate {
	return TemplateEstimate{AvgForkUniqueBytes: defaultForkUniqueFloorBytes}
}

// templateEstimatesFromReport derives one TemplateEstimate per template in the
// CoW-aware metering report. SharedOnceBytes comes straight from the template
// row's SharedOnce; AvgForkUniqueBytes is the sum of that template's forks'
// unique footprints divided by its fork count, floored to
// defaultForkUniqueFloorBytes when the template has no forks. snapshot digests
// are looked up from the provided map. Templates whose grouping key is empty
// (ungrouped sandboxes) are skipped: they carry no template identity to bill.
func templateEstimatesFromReport(report metering.Report, digests map[string]string) []TemplateEstimate {
	uniqueByTemplate := make(map[string]int64)
	for _, s := range report.Sandboxes {
		if s.Template == "" {
			continue
		}
		uniqueByTemplate[s.Template] += s.MemoryUnique
	}

	out := make([]TemplateEstimate, 0, len(report.Templates))
	for _, t := range report.Templates {
		if t.Template == "" {
			continue
		}
		avgUnique := defaultForkUniqueFloorBytes
		if t.ForkCount > 0 {
			avgUnique = uniqueByTemplate[t.Template] / int64(t.ForkCount)
		}
		out = append(out, TemplateEstimate{
			TemplateID:         t.Template,
			SnapshotDigest:     digests[t.Template],
			SharedOnceBytes:    t.SharedOnce,
			AvgForkUniqueBytes: avgUnique,
			ForkCount:          int32(t.ForkCount),
		})
	}
	return out
}
