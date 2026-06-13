// Package husk implements the husk-pod stub: a single-VM process that brings
// up a DORMANT Firecracker VMM at prepare time and ACTIVATES it in place by
// loading a snapshot when an activate request arrives over a control socket.
//
// One husk process owns exactly one VM. The activate path drives a VMM and is
// security sensitive: it FAILS CLOSED. Any snapshot-load or guest-readiness
// failure leaves the stub NOT active and reports an error; it never reports a
// usable VM it could not verify.
package husk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/paperclipinc/mitos/internal/firecracker"
	"github.com/paperclipinc/mitos/internal/vsock"
)

// ActivateRequest is the control message asking the dormant VMM to load a
// snapshot in place. SnapshotDir is the template snapshot directory; the stub
// reads the mem and vmstate files from it using the same layout the fork engine
// writes (SnapshotDir/mem and SnapshotDir/vmstate). NetworkOverrides remap the
// snapshot's baked placeholder NIC to this husk's tap, exactly as the engine
// fork path passes them.
//
// Env and Secrets are the claim-time guest configuration delivered after the
// restore handshake, mirroring the daemon's deliverConfig. Network and Volumes
// are the per-fork guest network and volume-mount table threaded into the
// NotifyForked handshake, for parity with the engine fork path.
//
// Secret VALUES are never logged anywhere in the control path: the codec moves
// them, but no log or error message ever prints them. In this PR the secrets
// ride the local control socket only; the real claim-time secret source is the
// controller, delivered to the husk pod's stub by the future controller-
// migration PR.
//
// Token is the per-sandbox bearer token the controller mints for this claim. The
// stub registers it as the gate on the in-pod sandbox HTTP API (exec/files) it
// serves after a successful activate, so only a caller presenting this token can
// reach the activated VM. It is a SECRET: it rides the mTLS control channel and
// is NEVER logged.
type ActivateRequest struct {
	SnapshotDir string `json:"snapshot_dir"`
	// ExpectedDigest is the template's recorded CAS manifest digest (a content
	// address, NOT a secret). The controller passes the digest forkd reported via
	// GetCapacity (the NodeRegistry TemplateDigests), and the stub re-verifies the
	// on-disk snapshot against it BEFORE loading: the mounted manifest must hash
	// to this digest and the loaded mem+vmstate must re-hash to the manifest, so a
	// snapshot tampered on the node disk is refused (fail-closed, the husk mirror
	// of forkd's verify-on-load gate, issues #9 and #32). It is safe to log but is
	// kept out of noisy logging. An empty digest is refused unless the stub runs
	// with the development --allow-unverified-snapshots escape hatch.
	ExpectedDigest   string                        `json:"expected_digest,omitempty"`
	NetworkOverrides []firecracker.NetworkOverride `json:"network_overrides,omitempty"`
	Env              map[string]string             `json:"env,omitempty"`
	Secrets          map[string]string             `json:"secrets,omitempty"`
	Network          *vsock.NotifyForkedNetwork    `json:"network,omitempty"`
	Volumes          []vsock.VolumeMountEntry      `json:"volumes,omitempty"`
	Token            string                        `json:"token,omitempty"`
}

// ActivateResult is the control reply. OK is true only when the snapshot loaded
// AND the guest agent answered over vsock. VsockPath is the host UDS path of the
// activated guest agent (only meaningful when OK). LatencyMs is the wall time
// from the activate call to guest readiness. Error carries actionable
// remediation text when OK is false; it never carries secrets.
type ActivateResult struct {
	OK        bool    `json:"ok"`
	VsockPath string  `json:"vsock_path,omitempty"`
	LatencyMs float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

// WriteRequest writes an ActivateRequest as one line of JSON followed by a
// newline. The line-delimited framing lets a peer ReadRequest one message
// without buffering the whole stream.
func WriteRequest(w io.Writer, req ActivateRequest) error {
	return writeLine(w, req)
}

// ReadRequest reads one line-delimited ActivateRequest from r.
func ReadRequest(r io.Reader) (ActivateRequest, error) {
	var req ActivateRequest
	err := readLine(r, &req)
	return req, err
}

// WriteResult writes an ActivateResult as one line of JSON followed by a
// newline.
func WriteResult(w io.Writer, res ActivateResult) error {
	return writeLine(w, res)
}

// ReadResult reads one line-delimited ActivateResult from r.
func ReadResult(r io.Reader) (ActivateResult, error) {
	var res ActivateResult
	err := readLine(r, &res)
	return res, err
}

func writeLine(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode control message: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write control message: %w", err)
	}
	return nil
}

func readLine(r io.Reader, v interface{}) error {
	scanner := bufio.NewScanner(r)
	// Control messages are tiny, but a snapshot dir plus override list is
	// unbounded in principle; allow a generous line so a long request is not
	// silently truncated.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read control message: %w", err)
		}
		return io.EOF
	}
	if err := json.Unmarshal(scanner.Bytes(), v); err != nil {
		return fmt.Errorf("decode control message: %w", err)
	}
	return nil
}
