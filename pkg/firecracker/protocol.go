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
