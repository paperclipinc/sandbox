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
	// Network, when set, attaches a NIC to the VM. It is used two ways:
	// for a FRESH boot (template creation, non-restore claims) the NIC is
	// bound to a live tap before InstanceStart; the engine fork path instead
	// passes the identity to LoadSnapshotWithOverrides so each fork remaps
	// the snapshot's baked placeholder NIC to its own tap. The zero value
	// keeps the prior NIC-less behavior. The fields are safe to log.
	Network *NetworkIdentity
}

// NetworkIdentity is the per-VM network binding StartVM applies: which guest
// NIC (IfaceID, GuestMAC) maps to which host tap (HostDevName). It mirrors the
// fields of netconf.Identity that Firecracker needs, kept here so the
// firecracker package does not import netconf. All fields are safe to log.
type NetworkIdentity struct {
	IfaceID     string
	GuestMAC    string
	HostDevName string
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
	// NetworkOverrides remaps each snapshot network interface to a fresh
	// host tap at load time. Firecracker (>= v1.12, pinned CI is v1.15)
	// accepts this optional array on PUT /snapshot/load so a single shared
	// snapshot can be forked many times with each fork bound to its OWN tap.
	// Omitted (nil) restores the device against its baked host_dev_name,
	// preserving the prior behavior for snapshots taken without a NIC.
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
}

// NetworkOverride remaps one snapshot network interface (identified by its
// IfaceID) to a different host tap (HostDevName) when the snapshot is loaded.
// It is the network analog of the relative vsock uds_path: it lets every fork
// of one snapshot bind a distinct, freshly created tap. All fields are safe to
// log (interface ids and tap names carry no secrets).
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// NetworkInterface is the PUT /network-interfaces/{iface_id} request body. It
// binds a guest NIC (IfaceID, GuestMAC) to a host tap device (HostDevName).
// The JSON field names match the Firecracker API exactly. All fields are safe
// to log: ids, MACs, and tap names carry no secrets.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac,omitempty"`
	HostDevName string `json:"host_dev_name"`
}
