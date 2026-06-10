package benchstat

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"testing"
	"time"
)

// makeSamples returns n samples with values 1ms, 2ms, ..., n*ms in order.
func makeSamples(n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		out[i] = time.Duration(i+1) * time.Millisecond
	}
	return out
}

func TestSummarizeKnownPercentiles(t *testing.T) {
	// 100 samples valued 1ms..100ms. Nearest-rank index = ceil(P/100*n)-1.
	// P50 -> ceil(50)-1 = 49 -> 50ms; P90 -> ceil(90)-1 = 89 -> 90ms;
	// P99 -> ceil(99)-1 = 98 -> 99ms.
	s := Summarize(makeSamples(100))

	if s.Count != 100 {
		t.Errorf("Count = %d, want 100", s.Count)
	}
	if s.Min != 1*time.Millisecond {
		t.Errorf("Min = %v, want 1ms", s.Min)
	}
	if s.Max != 100*time.Millisecond {
		t.Errorf("Max = %v, want 100ms", s.Max)
	}
	if s.P50 != 50*time.Millisecond {
		t.Errorf("P50 = %v, want 50ms", s.P50)
	}
	if s.P90 != 90*time.Millisecond {
		t.Errorf("P90 = %v, want 90ms", s.P90)
	}
	if s.P99 != 99*time.Millisecond {
		t.Errorf("P99 = %v, want 99ms", s.P99)
	}
	// Mean of 1..100 ms = 50.5ms.
	wantMean := 50500 * time.Microsecond
	if s.Mean != wantMean {
		t.Errorf("Mean = %v, want %v", s.Mean, wantMean)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil)
	if s != (Summary{}) {
		t.Errorf("Summarize(nil) = %+v, want zero Summary", s)
	}
	s2 := Summarize([]time.Duration{})
	if s2 != (Summary{}) {
		t.Errorf("Summarize(empty) = %+v, want zero Summary", s2)
	}
}

func TestSummarizeSingle(t *testing.T) {
	s := Summarize([]time.Duration{42 * time.Millisecond})
	v := 42 * time.Millisecond
	if s.Count != 1 {
		t.Errorf("Count = %d, want 1", s.Count)
	}
	for name, got := range map[string]time.Duration{
		"Min": s.Min, "P50": s.P50, "P90": s.P90,
		"P99": s.P99, "Max": s.Max, "Mean": s.Mean,
	} {
		if got != v {
			t.Errorf("%s = %v, want %v", name, got, v)
		}
	}
}

func TestSummarizeOrderingInvariant(t *testing.T) {
	ordered := makeSamples(100)
	shuffled := make([]time.Duration, len(ordered))
	copy(shuffled, ordered)
	r := rand.New(rand.NewSource(1))
	r.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	a := Summarize(ordered)
	b := Summarize(shuffled)
	if a != b {
		t.Errorf("shuffled summary differs: %+v vs %+v", a, b)
	}
}

func TestSummarizeDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{3 * time.Millisecond, 1 * time.Millisecond, 2 * time.Millisecond}
	_ = Summarize(in)
	if in[0] != 3*time.Millisecond || in[1] != 1*time.Millisecond || in[2] != 2*time.Millisecond {
		t.Errorf("input was mutated: %v", in)
	}
}

func TestTableContainsValues(t *testing.T) {
	s := Summarize(makeSamples(100))
	tbl := s.Table()
	for _, want := range []string{"count", "min", "p50", "p90", "p99", "max", "mean", "100"} {
		if !bytes.Contains([]byte(tbl), []byte(want)) {
			t.Errorf("Table() missing %q:\n%s", want, tbl)
		}
	}
}

func TestWriteJSONRoundTrip(t *testing.T) {
	results := []Result{
		{Name: "fork_to_first_exec", Unit: "ms", Summary: Summarize(makeSamples(100))},
		{Name: "exec_round_trip", Unit: "ms", Summary: Summarize(makeSamples(50))},
	}

	var buf bytes.Buffer
	if err := WriteJSON(&buf, results); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got []Result
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got) != len(results) {
		t.Fatalf("len = %d, want %d", len(got), len(results))
	}
	for i := range results {
		if got[i].Name != results[i].Name {
			t.Errorf("[%d].Name = %q, want %q", i, got[i].Name, results[i].Name)
		}
		if got[i].Unit != results[i].Unit {
			t.Errorf("[%d].Unit = %q, want %q", i, got[i].Unit, results[i].Unit)
		}
		if got[i].Summary != results[i].Summary {
			t.Errorf("[%d].Summary = %+v, want %+v", i, got[i].Summary, results[i].Summary)
		}
	}
}
