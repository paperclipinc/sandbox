package sandboxtable

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mkClaim(name, ns, pool string, phase v1alpha1.SandboxPhase, node, endpoint string, age time.Duration, now time.Time) v1alpha1.SandboxClaim {
	return v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(now.Add(-age)),
		},
		Spec: v1alpha1.SandboxClaimSpec{
			PoolRef: v1alpha1.LocalObjectReference{Name: pool},
		},
		Status: v1alpha1.SandboxClaimStatus{
			Phase:    phase,
			Node:     node,
			Endpoint: endpoint,
		},
	}
}

func TestFormatClaimsColumnsAndValues(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	claims := []v1alpha1.SandboxClaim{
		mkClaim("alpha", "default", "web", v1alpha1.SandboxReady, "node-1", "10.0.0.5:9091", 2*time.Minute, now),
		mkClaim("beta", "default", "batch", v1alpha1.SandboxPending, "", "", 3*time.Hour, now),
	}
	out := FormatClaims(claims, now)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 rows, got %d lines:\n%s", len(lines), out)
	}
	header := lines[0]
	for _, col := range []string{"NAME", "POOL", "PHASE", "NODE", "ENDPOINT", "AGE"} {
		if !strings.Contains(header, col) {
			t.Errorf("header missing column %q: %q", col, header)
		}
	}
	// Aligned columns: NAME starts at index 0, each subsequent column header
	// must appear at the same offset as its row values. Check field-by-field.
	fields := func(s string) []string { return strings.Fields(s) }

	row0 := fields(lines[1])
	want0 := []string{"alpha", "web", "Ready", "node-1", "10.0.0.5:9091", "2m"}
	for i := range want0 {
		if row0[i] != want0[i] {
			t.Errorf("row0 field %d = %q, want %q (row=%q)", i, row0[i], want0[i], lines[1])
		}
	}

	row1 := fields(lines[2])
	want1 := []string{"beta", "batch", "Pending", "-", "-", "3h"}
	for i := range want1 {
		if row1[i] != want1[i] {
			t.Errorf("row1 field %d = %q, want %q (row=%q)", i, row1[i], want1[i], lines[2])
		}
	}
}

func TestFormatClaimsEmpty(t *testing.T) {
	out := FormatClaims(nil, time.Now())
	if !strings.Contains(out, "No sandboxes found") {
		t.Errorf("empty claims should report no sandboxes, got %q", out)
	}
}

func TestFormatClaimsAgeUnits(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		age  time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{2 * time.Minute, "2m"},
		{3 * time.Hour, "3h"},
		{5 * 24 * time.Hour, "5d"},
		{0, "0s"},
	}
	for _, c := range cases {
		claims := []v1alpha1.SandboxClaim{mkClaim("x", "default", "p", v1alpha1.SandboxReady, "n", "e", c.age, now)}
		out := FormatClaims(claims, now)
		if !strings.Contains(out, c.want) {
			t.Errorf("age %v: want %q in output, got %q", c.age, c.want, out)
		}
	}
}

func TestFormatForksColumnsAndValues(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	forks := []v1alpha1.SandboxFork{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "fork-a",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
			},
			Spec: v1alpha1.SandboxForkSpec{
				SourceRef: v1alpha1.LocalObjectReference{Name: "claim-x"},
				Replicas:  3,
			},
			Status: v1alpha1.SandboxForkStatus{
				ReadyForks: 2,
				TotalForks: 3,
			},
		},
	}
	out := FormatForks(forks, now)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected header + 1 row, got %d:\n%s", len(lines), out)
	}
	for _, col := range []string{"NAME", "SOURCE", "READY", "AGE"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header missing %q: %q", col, lines[0])
		}
	}
	row := strings.Fields(lines[1])
	want := []string{"fork-a", "claim-x", "2/3", "10m"}
	for i := range want {
		if row[i] != want[i] {
			t.Errorf("fork row field %d = %q, want %q (row=%q)", i, row[i], want[i], lines[1])
		}
	}
}

func TestFormatForksEmpty(t *testing.T) {
	out := FormatForks(nil, time.Now())
	if !strings.Contains(out, "No forks found") {
		t.Errorf("empty forks should report no forks, got %q", out)
	}
}

func TestFormatForksMissingSource(t *testing.T) {
	now := time.Now()
	forks := []v1alpha1.SandboxFork{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "f", CreationTimestamp: metav1.NewTime(now)},
			Spec:       v1alpha1.SandboxForkSpec{Replicas: 1},
		},
	}
	out := FormatForks(forks, now)
	row := strings.Fields(strings.Split(strings.TrimRight(out, "\n"), "\n")[1])
	if row[1] != "-" {
		t.Errorf("missing source should be dash, got %q", row[1])
	}
}
