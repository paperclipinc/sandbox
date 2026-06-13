package fork

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paperclipinc/mitos/internal/netconf"
)

// journalDirName is the per-VM journal subdirectory under the data dir. Each
// live sandbox writes one <id>.json record here at create and removes it at
// clean destroy, so a restarted forkd can recognize and reap its own pre-crash
// VMs (issue #12). It is the same "sandboxes" tree that holds each VM's working
// directory (<dataDir>/sandboxes/<id>/); the record is a sibling FILE
// (<dataDir>/sandboxes/<id>.json), so the two never collide. The records carry
// ids, pids, and host paths ONLY: never env, secrets, or tokens.
const journalDirName = "sandboxes"

// networkIdentity is the JSON-serializable form of netconf.Identity. net.IP is
// stored as its string form so the record is human-readable and stable across
// restarts. All fields are network configuration, safe to persist and log.
type networkIdentity struct {
	TapName  string `json:"tapName"`
	GuestMAC string `json:"guestMAC"`
	HostIP   string `json:"hostIP"`
	GuestIP  string `json:"guestIP"`
}

// networkIdentityFrom converts a live netconf.Identity into its persisted form.
func networkIdentityFrom(id netconf.Identity) networkIdentity {
	ni := networkIdentity{TapName: id.TapName, GuestMAC: id.GuestMAC}
	if id.HostIP != nil {
		ni.HostIP = id.HostIP.String()
	}
	if id.GuestIP != nil {
		ni.GuestIP = id.GuestIP.String()
	}
	return ni
}

// toIdentity reconstructs the netconf.Identity used for network teardown. A
// blank or unparsable IP yields a nil net.IP, which the teardown path tolerates.
func (n networkIdentity) toIdentity() netconf.Identity {
	id := netconf.Identity{TapName: n.TapName, GuestMAC: n.GuestMAC}
	if n.HostIP != "" {
		id.HostIP = net.ParseIP(n.HostIP).To4()
	}
	if n.GuestIP != "" {
		id.GuestIP = net.ParseIP(n.GuestIP).To4()
	}
	return id
}

// sandboxRecord is the on-disk journal record for one live sandbox. It carries
// EXACTLY what startup reconcile needs to either re-adopt a still-running VM or
// reap a dead one's leaked artifacts (chroot, CoW rootfs clone, fork network,
// jailer uid). It holds ids, the Firecracker pid, and host paths only; it never
// carries env, secrets, or tokens (the secrets rule).
type sandboxRecord struct {
	ID         string    `json:"id"`
	TemplateID string    `json:"templateID"`
	SnapshotID string    `json:"snapshotID"`
	Endpoint   string    `json:"endpoint"`
	Pid        int       `json:"pid"`
	CreatedAt  time.Time `json:"createdAt"`
	VsockPath  string    `json:"vsockPath"`
	// RootfsPath is the per-activation rootfs the snapshot was restored from.
	RootfsPath string `json:"rootfsPath"`
	// ChrootDir is the jailer chroot root (empty for direct-exec VMs). Reaping a
	// dead VM removes JailerVMDir, which contains it.
	ChrootDir string `json:"chrootDir"`
	// JailerVMDir is the per-VM jailer workspace (parent of ChrootDir); reaping a
	// dead jailed VM removes this whole tree.
	JailerVMDir string `json:"jailerVMDir"`
	// JailedUID is the dedicated uid the jailer ran the VM under (0 for
	// direct-exec). Reaping a dead jailed VM returns it to the allocator pool.
	JailedUID uint32 `json:"jailedUID"`
	// Network is this fork's per-fork network identity (zero TapName when
	// networking was disabled or the fork carried none).
	Network networkIdentity `json:"network"`
	// HasVolumes records whether per-fork volume backings were prepared, so
	// reaping cleans them up.
	HasVolumes bool `json:"hasVolumes"`
	// FirecrackerBin is the firecracker binary path this VM was launched with. The
	// startup PID-recycle guard checks the live pid's executable against it so a
	// recycled, unrelated pid is never adopted or killed as ours.
	FirecrackerBin string `json:"firecrackerBin"`
}

// journal persists sandbox records under <dataDir>/sandbox-journal. Writes are
// atomic (temp file + rename) so a crash mid-write never leaves a torn record.
type journal struct {
	dir string
}

// newJournal returns a journal rooted at <dataDir>/sandbox-journal.
func newJournal(dataDir string) *journal {
	return &journal{dir: filepath.Join(dataDir, journalDirName)}
}

// recordPath returns the on-disk path of one sandbox's record.
func (j *journal) recordPath(id string) string {
	return filepath.Join(j.dir, id+".json")
}

// write atomically persists a record. It creates the journal dir if needed and
// writes to a temp file that is renamed into place, so a reader (or a crash)
// never observes a partial record.
func (j *journal) write(rec sandboxRecord) error {
	if err := os.MkdirAll(j.dir, 0o755); err != nil {
		return fmt.Errorf("create journal dir: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal journal record %s: %w", rec.ID, err)
	}
	tmp, err := os.CreateTemp(j.dir, rec.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp journal record %s: %w", rec.ID, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp journal record %s: %w", rec.ID, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp journal record %s: %w", rec.ID, err)
	}
	if err := os.Rename(tmpName, j.recordPath(rec.ID)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit journal record %s: %w", rec.ID, err)
	}
	return nil
}

// remove deletes a sandbox's record. Removing an absent record is not an error
// (idempotent), so a double-Terminate or a record already reaped at startup is
// harmless.
func (j *journal) remove(id string) error {
	if err := os.Remove(j.recordPath(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove journal record %s: %w", id, err)
	}
	return nil
}

// load reads every journal record. A record that fails to parse is skipped (the
// reconcile path must fail open: one bad record must not stop forkd from
// starting) and reported via the returned bad-file list so the caller can log
// it. Returns an empty slice when the journal dir does not yet exist.
func (j *journal) load() ([]sandboxRecord, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read journal dir: %w", err)
	}
	recs := make([]sandboxRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(j.dir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "forkd: skip unreadable journal record %s: %v\n", e.Name(), err)
			continue
		}
		var rec sandboxRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			fmt.Fprintf(os.Stderr, "forkd: skip malformed journal record %s: %v\n", e.Name(), err)
			continue
		}
		recs = append(recs, rec)
	}
	return recs, nil
}
