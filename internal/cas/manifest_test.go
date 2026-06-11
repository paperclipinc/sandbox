package cas

import (
	"bytes"
	"testing"
)

func TestBuildManifestDeterministicDigest(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a", bytes.Repeat([]byte{0x11}, 5<<20))
	b := writeFile(t, dir, "b", bytes.Repeat([]byte{0x22}, 3<<20))

	m1, err := BuildManifest(map[string]string{"mem": a, "disk": b}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	m2, err := BuildManifest(map[string]string{"mem": a, "disk": b}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m1.Digest() != m2.Digest() {
		t.Fatalf("same inputs produced different digests: %s vs %s", m1.Digest(), m2.Digest())
	}
}

func TestBuildManifestInputMapOrderInvariant(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a", bytes.Repeat([]byte{0x11}, 5<<20))
	b := writeFile(t, dir, "b", bytes.Repeat([]byte{0x22}, 3<<20))

	m1, err := BuildManifest(map[string]string{"mem": a, "disk": b}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	// Same logical content, different insertion order.
	m2, err := BuildManifest(map[string]string{"disk": b, "mem": a}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m1.Digest() != m2.Digest() {
		t.Fatalf("input map reorder changed digest: %s vs %s", m1.Digest(), m2.Digest())
	}
	if !bytes.Equal(m1.Canonical(), m2.Canonical()) {
		t.Fatalf("canonical encodings differ across map order")
	}
}

func TestBuildManifestChangedByteChangesDigest(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x11}, 5<<20)
	a := writeFile(t, dir, "a", data)

	m1, err := BuildManifest(map[string]string{"mem": a}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	changed := append([]byte(nil), data...)
	changed[len(changed)-1] ^= 0xFF
	a2 := writeFile(t, dir, "a", changed)
	m2, err := BuildManifest(map[string]string{"mem": a2}, Metadata{VMMVersion: "v1", CreatedUnix: 1000})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m1.Digest() == m2.Digest() {
		t.Fatalf("changed byte did not change manifest digest")
	}
}

func TestManifestCanonicalSortsFilesByName(t *testing.T) {
	m := Manifest{
		Files: []FileEntry{
			{Name: "zeta", Size: 1},
			{Name: "alpha", Size: 2},
		},
		VMMVersion:  "v1",
		CreatedUnix: 1,
	}
	canon := m.Canonical()
	ai := bytes.Index(canon, []byte("alpha"))
	zi := bytes.Index(canon, []byte("zeta"))
	if ai < 0 || zi < 0 || ai > zi {
		t.Fatalf("Canonical did not sort files by name: %s", canon)
	}
}

func TestManifestCanonicalDeterministicWithEnvFields(t *testing.T) {
	mk := func() Manifest {
		return Manifest{
			Files:                 []FileEntry{{Name: "mem", Size: 3}},
			VMMVersion:            "1.15.0",
			CreatedUnix:           42,
			SnapshotFormatVersion: CurrentSnapshotFormatVersion,
			CPUModel:              "Intel(R) Xeon(R) CPU @ 2.20GHz",
			KernelVersion:         "6.1.0",
			ConfigHash:            "abc123",
		}
	}
	first, second := mk(), mk()
	if !bytes.Equal(first.Canonical(), second.Canonical()) {
		t.Fatal("same content produced different canonical bytes")
	}
	if first.Digest() != second.Digest() {
		t.Fatal("same content produced different digest")
	}
}

func TestManifestEnvFieldsChangeDigest(t *testing.T) {
	base := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 3}},
		SnapshotFormatVersion: 1,
		CPUModel:              "cpuA",
		KernelVersion:         "kA",
		ConfigHash:            "hA",
	}
	for _, mut := range []func(*Manifest){
		func(m *Manifest) { m.SnapshotFormatVersion = 2 },
		func(m *Manifest) { m.CPUModel = "cpuB" },
		func(m *Manifest) { m.KernelVersion = "kB" },
		func(m *Manifest) { m.ConfigHash = "hB" },
	} {
		changed := base
		mut(&changed)
		if base.Digest() == changed.Digest() {
			t.Fatal("changing an env field did not change the digest")
		}
	}
}

func TestManifestRoundTripEnvFields(t *testing.T) {
	m := Manifest{
		Files:                 []FileEntry{{Name: "mem", Size: 7, Chunks: []ChunkRef{{Digest: digestBytes([]byte("x")), Size: 1}}}},
		VMMVersion:            "1.15.0",
		CreatedUnix:           99,
		SnapshotFormatVersion: CurrentSnapshotFormatVersion,
		CPUModel:              "Intel(R) Xeon(R) CPU @ 2.20GHz",
		KernelVersion:         "6.1.0",
		ConfigHash:            "deadbeef",
	}
	got, err := decodeManifest(m.Canonical())
	if err != nil {
		t.Fatalf("decodeManifest: %v", err)
	}
	if got.SnapshotFormatVersion != m.SnapshotFormatVersion ||
		got.CPUModel != m.CPUModel ||
		got.KernelVersion != m.KernelVersion ||
		got.ConfigHash != m.ConfigHash ||
		got.VMMVersion != m.VMMVersion ||
		got.CreatedUnix != m.CreatedUnix {
		t.Fatalf("round-trip lost env fields: got %+v want %+v", got, m)
	}
	// A decoded manifest must re-derive the same digest.
	if got.Digest() != m.Digest() {
		t.Fatalf("round-trip digest mismatch: %s vs %s", got.Digest(), m.Digest())
	}
}
