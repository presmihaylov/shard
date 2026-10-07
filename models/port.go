package models

// PortForward carries TCP from one host port to one port inside the sandbox, on 127.0.0.1 or, when Public, on 0.0.0.0.
type PortForward struct {
	HostPort  uint16 `json:"host_port" minimum:"1" maximum:"65535" doc:"The port on the host the forward listens on."`
	GuestPort uint16 `json:"guest_port" minimum:"1" maximum:"65535" doc:"The port on the sandbox's 127.0.0.1 a connection goes to."`
	Public    bool   `json:"public,omitempty" doc:"Listen on 0.0.0.0, every interface of the host, instead of 127.0.0.1."`
}

// Port is one forward as the host serves it now: the record's mapping, and whether its listener is up.
type Port struct {
	Sandbox     string `json:"sandbox" doc:"The sandbox id."`
	SandboxName string `json:"sandbox_name,omitempty"`
	HostPort    uint16 `json:"host_port" minimum:"1" maximum:"65535"`
	GuestPort   uint16 `json:"guest_port" minimum:"1" maximum:"65535"`
	Public      bool   `json:"public"`
	Address     string `json:"address" doc:"What the listener binds: 127.0.0.1, or 0.0.0.0 for a public forward."`
	Listening   bool   `json:"listening" doc:"False while the sandbox is not running, or while the host refuses the port."`
	Error       string `json:"error,omitempty" doc:"Why a running sandbox's port does not listen, or else what the last connection hit."`
	// ReachableOn is empty while the port does not listen, and less the sandbox bridge for a public one.
	ReachableOn []HostAddress `json:"reachable_on" doc:"The IPv4 addresses of the host a client reaches the port on while it listens."`
}

// HostAddress is one IPv4 address of the host and the interface it is on.
type HostAddress struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
}
