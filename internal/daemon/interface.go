package daemon

import (
	"context"

	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/metering"
	"github.com/paperclipinc/mitos/internal/volume"
)

// ForkEngine is the interface both the real Firecracker engine
// and the mock engine implement.
type ForkEngine interface {
	Fork(snapshotID, sandboxID string, opts fork.ForkOpts) (*fork.ForkResult, error)
	ForkRunning(sourceSandboxID, newSandboxID string, pauseSource bool) (*fork.ForkResult, error)
	Terminate(sandboxID string) error
	GetCapacity() fork.Capacity
	// Metering returns the full CoW-aware metering report (per-sandbox and
	// per-template memory plus disk) for the operator/billing endpoint. Unlike
	// GetCapacity it is NOT on the fork hot path and may stat backing files.
	Metering() metering.Report
	ListSandboxes() []fork.SandboxRecord
	// CreateTemplate builds a template snapshot. volumes are the template's
	// declared volumes; the engine bakes one placeholder drive per volume into
	// the snapshot. Nil leaves the template drive-less (only the rootfs).
	CreateTemplate(id string, image string, initCommands []string, volumes []volume.Spec) error
	// PullTemplate fetches a template's snapshot from a peer forkd's CAS over
	// the peer's token-gated TLS surface, materializes it, verifies it, and
	// records the digest. token is a credential and must never be logged.
	PullTemplate(ctx context.Context, templateID, manifestDigest, sourceURL, token string) error
}
