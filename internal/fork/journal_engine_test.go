package fork

import (
	"net"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/netconf"
)

// TestEngineJournalsSandboxOnCreate checks that journalSandbox persists a record
// carrying the fields reconcile needs, and that unjournalSandbox removes it on
// clean destroy.
func TestEngineJournalsSandboxOnCreate(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{
		dataDir:        dir,
		firecrackerBin: "/usr/bin/firecracker",
		journal:        newJournal(dir),
	}

	sb := &Sandbox{
		ID:         "sbx-7",
		TemplateID: "tmpl-x",
		SnapshotID: "tmpl-x",
		Endpoint:   "127.0.0.1:10007",
		Pid:        9988,
		CreatedAt:  time.Unix(1700000000, 0).UTC(),
		VsockPath:  "/data/sandboxes/sbx-7/vsock.sock",
		rootfsPath: "/data/templates/tmpl-x/rootfs.ext4",
		netID: netconf.Identity{
			TapName: "sbtap-7",
			HostIP:  net.IPv4(10, 200, 0, 1).To4(),
			GuestIP: net.IPv4(10, 200, 0, 2).To4(),
		},
		hasVolumes: true,
	}

	e.journalSandbox(sb)

	recs, err := e.journal.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 journal record after create, got %d", len(recs))
	}
	rec := recs[0]
	if rec.ID != sb.ID || rec.Pid != sb.Pid || rec.RootfsPath != sb.rootfsPath {
		t.Fatalf("record missing core fields: %+v", rec)
	}
	if rec.Network.TapName != "sbtap-7" || !rec.HasVolumes {
		t.Fatalf("record missing identity/volume fields: %+v", rec)
	}
	if rec.FirecrackerBin != "/usr/bin/firecracker" {
		t.Fatalf("record missing firecracker binary path: %+v", rec)
	}

	e.unjournalSandbox(sb.ID)
	recs, err = e.journal.load()
	if err != nil {
		t.Fatalf("load after destroy: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("journal record not removed on clean destroy: %d remain", len(recs))
	}
}

// TestEngineJournalNilIsNoOp checks that an engine with no journal (e.g. paths
// that never set one) does not panic when journaling.
func TestEngineJournalNilIsNoOp(t *testing.T) {
	e := &Engine{}
	e.journalSandbox(&Sandbox{ID: "x"})
	e.unjournalSandbox("x")
}
