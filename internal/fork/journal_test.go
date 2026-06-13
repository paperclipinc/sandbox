package fork

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/netconf"
)

func sampleRecord(id string) sandboxRecord {
	return sandboxRecord{
		ID:          id,
		TemplateID:  "tmpl-a",
		SnapshotID:  "tmpl-a",
		Endpoint:    "127.0.0.1:10000",
		Pid:         4242,
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
		VsockPath:   "/data/sandboxes/" + id + "/vsock.sock",
		RootfsPath:  "/data/templates/tmpl-a/rootfs.ext4",
		ChrootDir:   "/srv/jail/firecracker/" + id + "/root",
		JailerVMDir: "/srv/jail/firecracker/" + id,
		JailedUID:   100123,
		Network: networkIdentity{
			TapName:  "fctap-deadbeef",
			GuestMAC: "02:00:00:de:ad:be",
			HostIP:   "10.200.0.1",
			GuestIP:  "10.200.0.2",
		},
		HasVolumes:     true,
		FirecrackerBin: "/usr/bin/firecracker",
	}
}

func TestJournalWriteLoadRemove(t *testing.T) {
	dir := t.TempDir()
	j := newJournal(dir)

	rec := sampleRecord("sbx-1")
	if err := j.write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The record lands at <dataDir>/sandboxes/<id>.json.
	want := filepath.Join(dir, "sandboxes", "sbx-1.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("record file %s not written: %v", want, err)
	}

	recs, err := j.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	got := recs[0]
	if got.ID != rec.ID || got.Pid != rec.Pid || got.ChrootDir != rec.ChrootDir {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, rec)
	}
	if got.Network.TapName != rec.Network.TapName || got.JailedUID != rec.JailedUID {
		t.Fatalf("identity round-trip mismatch: %+v", got.Network)
	}

	if err := j.remove("sbx-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("record file still present after remove: %v", err)
	}
	// Remove is idempotent: removing an absent record is not an error.
	if err := j.remove("sbx-1"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
}

func TestJournalIdentityConversion(t *testing.T) {
	id := netconf.Identity{
		TapName:  "fctap-1234",
		GuestMAC: "02:00:00:00:00:01",
		HostIP:   net.IPv4(10, 200, 0, 1).To4(),
		GuestIP:  net.IPv4(10, 200, 0, 2).To4(),
	}
	ni := networkIdentityFrom(id)
	back := ni.toIdentity()
	if back.TapName != id.TapName || back.GuestMAC != id.GuestMAC {
		t.Fatalf("tap/mac round-trip mismatch: %+v", back)
	}
	if !back.HostIP.Equal(id.HostIP) || !back.GuestIP.Equal(id.GuestIP) {
		t.Fatalf("ip round-trip mismatch: host=%v guest=%v", back.HostIP, back.GuestIP)
	}
}
