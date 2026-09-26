package firecracker

// The request and reply bodies of the firecracker API, the fields shard uses of each.

type machineConfig struct {
	VCPUs     int64 `json:"vcpu_count"`
	MemoryMiB int64 `json:"mem_size_mib"`
}

type bootSource struct {
	Kernel string `json:"kernel_image_path"`
	Initrd string `json:"initrd_path,omitempty"`
	Args   string `json:"boot_args"`
}

// drive never claims the root: the guest assembles its own from the devices, so root= stays out of the command line.
type drive struct {
	ID       string `json:"drive_id"`
	Path     string `json:"path_on_host"`
	Root     bool   `json:"is_root_device"`
	ReadOnly bool   `json:"is_read_only"`
}

type networkInterface struct {
	ID      string `json:"iface_id"`
	HostDev string `json:"host_dev_name"`
	MAC     string `json:"guest_mac,omitempty"`
}

type vsockDevice struct {
	CID  int    `json:"guest_cid"`
	Path string `json:"uds_path"`
}

type action struct {
	Type string `json:"action_type"`
}

type instance struct {
	ID      string `json:"id"`
	State   State  `json:"state"`
	Version string `json:"vmm_version"`
}

type fault struct {
	Message string `json:"fault_message"`
}

// vmState is the one patch the vCPUs take: Paused or Resumed, which is not the Running the instance then reports.
type vmState struct {
	State string `json:"state"`
}

type snapshotCreate struct {
	Type       string `json:"snapshot_type"`
	StatePath  string `json:"snapshot_path"`
	MemoryPath string `json:"mem_file_path"`
}

// snapshotLoad names the tap and the vsock path of the new process; the drives keep the paths the snapshot holds until a patch swaps them.
type snapshotLoad struct {
	StatePath     string            `json:"snapshot_path"`
	Memory        memoryBackend     `json:"mem_backend"`
	ResumeVM      bool              `json:"resume_vm"`
	Network       []networkOverride `json:"network_overrides,omitempty"`
	Vsock         *vsockOverride    `json:"vsock_override,omitempty"`
	ClockRealtime bool              `json:"clock_realtime"`
}

type memoryBackend struct {
	Type string `json:"backend_type"`
	Path string `json:"backend_path"`
}

type networkOverride struct {
	ID      string `json:"iface_id"`
	HostDev string `json:"host_dev_name"`
}

type vsockOverride struct {
	Path string `json:"uds_path"`
}

type partialDrive struct {
	ID   string `json:"drive_id"`
	Path string `json:"path_on_host"`
}
