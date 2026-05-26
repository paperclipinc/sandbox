package daemon

import "github.com/paperclipinc/sandbox/internal/fork"

// ForkEngine is the interface both the real Firecracker engine
// and the mock engine implement.
type ForkEngine interface {
	Fork(snapshotID, sandboxID string, opts fork.ForkOpts) (*fork.ForkResult, error)
	Terminate(sandboxID string) error
	GetCapacity() fork.Capacity
	CreateTemplate(id string, rootfsPath string, initWaitSecs int) error
}
