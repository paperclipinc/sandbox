// Package benchstat provides pure latency statistics for the bench driver:
// nearest-rank percentile summarization, a human-readable table, and a
// JSON-serializable result view. It has no runtime dependencies and is fully
// unit-tested so the timing path in cmd/bench carries no statistical logic.
package benchstat

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Summary is the distribution of a set of latency samples. Percentiles use the
// nearest-rank method (see Summarize).
type Summary struct {
	Count int
	Min   time.Duration
	P50   time.Duration
	P90   time.Duration
	P99   time.Duration
	Max   time.Duration
	Mean  time.Duration
}

// Summarize computes a Summary over samples. The input slice is not mutated; a
// sorted copy is taken internally.
//
// Percentiles use the nearest-rank method: for the P-th percentile of n sorted
// samples the chosen index is ceil(P/100 * n) - 1, clamped to [0, n-1]. An
// empty input yields the zero Summary. A single sample yields a Summary whose
// Min, percentiles, Max, and Mean are all that sample.
func Summarize(samples []time.Duration) Summary {
	n := len(samples)
	if n == 0 {
		return Summary{}
	}

	sorted := make([]time.Duration, n)
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, s := range sorted {
		total += s
	}

	return Summary{
		Count: n,
		Min:   sorted[0],
		P50:   percentile(sorted, 50),
		P90:   percentile(sorted, 90),
		P99:   percentile(sorted, 99),
		Max:   sorted[n-1],
		Mean:  total / time.Duration(n),
	}
}

// percentile returns the p-th percentile of the already-sorted slice using the
// nearest-rank method. sorted must be non-empty.
func percentile(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	idx := int(math.Ceil(p/100*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx > n-1 {
		idx = n - 1
	}
	return sorted[idx]
}

// Table renders the Summary as an aligned human-readable table with values in
// milliseconds.
func (s Summary) Table() string {
	rows := []struct {
		label string
		value string
	}{
		{"count", fmt.Sprintf("%d", s.Count)},
		{"min", ms(s.Min)},
		{"p50", ms(s.P50)},
		{"p90", ms(s.P90)},
		{"p99", ms(s.P99)},
		{"max", ms(s.Max)},
		{"mean", ms(s.Mean)},
	}

	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-6s %10s\n", r.label, r.value)
	}
	return b.String()
}

// ms formats a duration as milliseconds with three decimal places.
func ms(d time.Duration) string {
	return fmt.Sprintf("%.3f ms", float64(d)/float64(time.Millisecond))
}

// Result names a measured Summary and its unit (for example "ms").
type Result struct {
	Name    string
	Unit    string
	Summary Summary
}

// jsonSummary is the wire view of a Summary. Durations are exported as
// integer nanoseconds (the native time.Duration unit) so the JSON round-trips
// losslessly back into a time.Duration.
type jsonSummary struct {
	Count  int   `json:"count"`
	MinNs  int64 `json:"min_ns"`
	P50Ns  int64 `json:"p50_ns"`
	P90Ns  int64 `json:"p90_ns"`
	P99Ns  int64 `json:"p99_ns"`
	MaxNs  int64 `json:"max_ns"`
	MeanNs int64 `json:"mean_ns"`
}

type jsonResult struct {
	Name    string      `json:"name"`
	Unit    string      `json:"unit"`
	Summary jsonSummary `json:"summary"`
}

func toJSON(s Summary) jsonSummary {
	return jsonSummary{
		Count:  s.Count,
		MinNs:  s.Min.Nanoseconds(),
		P50Ns:  s.P50.Nanoseconds(),
		P90Ns:  s.P90.Nanoseconds(),
		P99Ns:  s.P99.Nanoseconds(),
		MaxNs:  s.Max.Nanoseconds(),
		MeanNs: s.Mean.Nanoseconds(),
	}
}

func fromJSON(j jsonSummary) Summary {
	return Summary{
		Count: j.Count,
		Min:   time.Duration(j.MinNs),
		P50:   time.Duration(j.P50Ns),
		P90:   time.Duration(j.P90Ns),
		P99:   time.Duration(j.P99Ns),
		Max:   time.Duration(j.MaxNs),
		Mean:  time.Duration(j.MeanNs),
	}
}

// MarshalJSON encodes the Result via its nanosecond wire view.
func (r Result) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonResult{Name: r.Name, Unit: r.Unit, Summary: toJSON(r.Summary)})
}

// UnmarshalJSON decodes a Result from its nanosecond wire view.
func (r *Result) UnmarshalJSON(data []byte) error {
	var j jsonResult
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	r.Name = j.Name
	r.Unit = j.Unit
	r.Summary = fromJSON(j.Summary)
	return nil
}

// WriteJSON writes results as indented JSON to w.
func WriteJSON(w io.Writer, results []Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		return fmt.Errorf("encode results: %w", err)
	}
	return nil
}
