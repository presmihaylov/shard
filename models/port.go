package models

// PortForward carries TCP from one host port to one port inside the sandbox, on 127.0.0.1 or, when Public, on 0.0.0.0.
type PortForward struct {
	HostPort  uint16 `json:"host_port"`
	GuestPort uint16 `json:"guest_port"`
	Public    bool   `json:"public,omitempty"`
}

// Port is one forward as the host serves it now: the record's mapping, and whether its listener is up.
type Port struct {
	Sandbox     string `json:"sandbox"`
	SandboxName string `json:"sandbox_name,omitempty"`
	HostPort    uint16 `json:"host_port"`
	GuestPort   uint16 `json:"guest_port"`
	Public      bool   `json:"public"`
	// Address is what the listener binds: 127.0.0.1, or 0.0.0.0 for a public forward.
	Address string `json:"address"`
	// Listening is false while the sandbox is not running, or while the host refuses the port.
	Listening bool `json:"listening"`
	// Error is why a running sandbox's port does not listen, or else what the last connection hit.
	Error string `json:"error,omitempty"`
	// ReachableOn is where a client finds the port while it listens.
	ReachableOn []HostAddress `json:"reachable_on"`
}

// HostAddress is one IPv4 address of the host and the interface it is on.
type HostAddress struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
}
