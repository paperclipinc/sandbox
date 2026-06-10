package firecracker

type VMConfig struct {
	ID             string
	FirecrackerBin string
	WorkDir        string
	SocketPath     string
	KernelPath     string
	RootfsPath     string
	VcpuCount      int
	MemSizeMib     int
	BootArgs       string
	// Jailer configures launching through the jailer binary. The zero
	// value keeps the direct-exec behavior.
	Jailer JailerConfig
	// ChrootFiles lists host files (kernel, rootfs, snapshot mem and
	// vmstate) that prepareChroot hard-links into the VM's chroot before
	// launch. Ignored when the jailer is disabled.
	ChrootFiles []string
}

func DefaultVMConfig() VMConfig {
	return VMConfig{
		FirecrackerBin: "/usr/local/bin/firecracker",
		VcpuCount:      1,
		MemSizeMib:     512,
		BootArgs:       "console=ttyS0 reboot=k panic=1 pci=off",
	}
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type MachineConfig struct {
	VcpuCount  int `json:"vcpu_count"`
	MemSizeMib int `json:"mem_size_mib"`
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsReadOnly   bool   `json:"is_read_only"`
	IsRootDevice bool   `json:"is_root_device"`
}

type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UdsPath  string `json:"uds_path"`
}

type Action struct {
	ActionType string `json:"action_type"`
}

type VMState struct {
	State string `json:"state"`
}

type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type SnapshotLoad struct {
	SnapshotPath        string `json:"snapshot_path"`
	MemFilePath         string `json:"mem_file_path"`
	EnableDiffSnapshots bool   `json:"enable_diff_snapshots"`
	ResumeVM            bool   `json:"resume_vm"`
}
