package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/metering"
	"github.com/paperclipinc/mitos/internal/volume"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// readGauge extracts the current value of a registered prometheus gauge.
func readGauge(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

const mib = int64(1024 * 1024)

// fakeMeteringEngine reports a fixed metering report and panics on any other
// ForkEngine call so the metering tests stay focused on the report path.
type fakeMeteringEngine struct {
	report metering.Report
}

func (f *fakeMeteringEngine) Metering() metering.Report { return f.report }

func (f *fakeMeteringEngine) Fork(string, string, fork.ForkOpts) (*fork.ForkResult, error) {
	panic("not used")
}
func (f *fakeMeteringEngine) ForkRunning(string, string, bool) (*fork.ForkResult, error) {
	panic("not used")
}
func (f *fakeMeteringEngine) Terminate(string) error { panic("not used") }
func (f *fakeMeteringEngine) GetCapacity() fork.Capacity {
	return fork.Capacity{ActiveSandboxes: int32(len(f.report.Sandboxes))}
}
func (f *fakeMeteringEngine) ListSandboxes() []fork.SandboxRecord { return nil }
func (f *fakeMeteringEngine) CreateTemplate(string, string, []string, []volume.Spec) error {
	panic("not used")
}
func (f *fakeMeteringEngine) PullTemplate(context.Context, string, string, string, string) error {
	panic("not used")
}

// sampleReport builds a known CoW-aware report: ten forks of template "A"
// (256 MiB shared, 1 MiB unique each) plus disk seeds counted once.
func sampleReport() metering.Report {
	samples := make([]metering.Sample, 0, 10)
	for i := 0; i < 10; i++ {
		samples = append(samples, metering.Sample{
			ID:           string(rune('a' + i)),
			Template:     "A",
			MemoryShared: 256 * mib,
			MemoryUnique: 1 * mib,
			DiskShared:   512 * mib,
			DiskUnique:   5 * mib,
		})
	}
	return metering.Aggregate(samples)
}

// TestMeteringEndpointReturnsReport verifies GET /v1/metering returns the
// aggregated CoW-aware report as JSON, reachable on the operator path with no
// per-sandbox bearer token.
func TestMeteringEndpointReturnsReport(t *testing.T) {
	engine := &fakeMeteringEngine{report: sampleReport()}
	ts := httptest.NewServer(meteringHandler(engine))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/metering")
	if err != nil {
		t.Fatalf("GET /v1/metering: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no per-sandbox token required)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	var got metering.Report
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode report: %v", err)
	}

	if want := 10*mib + 256*mib; got.UsedCoWAware != want {
		t.Errorf("UsedCoWAware = %d, want %d", got.UsedCoWAware, want)
	}
	if want := 256 * mib; got.SharedOnceTotal() != want {
		t.Errorf("SharedOnceTotal = %d, want %d", got.SharedOnceTotal(), want)
	}
	if want := 50*mib + 512*mib; got.DiskUsedCoWAware != want {
		t.Errorf("DiskUsedCoWAware = %d, want %d (seed counted once)", got.DiskUsedCoWAware, want)
	}
	if len(got.Templates) != 1 || got.Templates[0].ForkCount != 10 {
		t.Errorf("templates wrong: %+v", got.Templates)
	}
	if len(got.Sandboxes) != 10 {
		t.Errorf("sandboxes len = %d, want 10", len(got.Sandboxes))
	}
}

// TestMeteringEndpointNoSandboxToken proves the endpoint ignores per-sandbox
// bearer tokens entirely: it is operator data and a (irrelevant) bearer header
// neither grants nor blocks access.
func TestMeteringEndpointNoSandboxToken(t *testing.T) {
	engine := &fakeMeteringEngine{report: sampleReport()}
	ts := httptest.NewServer(meteringHandler(engine))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/metering", nil)
	req.Header.Set("Authorization", "Bearer some-sandbox-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestUpdateMetricsCoWAware verifies the gauges reflect CoW-aware values: the
// shared gauge is the shared-once total (NOT 10x the per-fork shared), unique
// is the per-fork total, the savings gauge is naive minus CoW-aware, and the
// disk gauge is the CoW-aware metered disk.
func TestUpdateMetricsCoWAware(t *testing.T) {
	engine := &fakeMeteringEngine{report: sampleReport()}
	srv := NewServer(engine, nil)
	srv.UpdateMetrics()

	if got := readGauge(t, memoryShared); got != float64(256*mib) {
		t.Errorf("memory_shared = %v, want %v (counted once)", got, float64(256*mib))
	}
	if got := readGauge(t, memoryUnique); got != float64(10*mib) {
		t.Errorf("memory_unique = %v, want %v", got, float64(10*mib))
	}
	if got := readGauge(t, cowMemorySavings); got != float64(2304*mib) {
		t.Errorf("cow_memory_savings = %v, want %v", got, float64(2304*mib))
	}
	if got := readGauge(t, meteredDisk); got != float64(50*mib+512*mib) {
		t.Errorf("metered_disk = %v, want %v", got, float64(50*mib+512*mib))
	}
}
