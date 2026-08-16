// Package models holds the domain types of shard: the sandbox, its state
// machine, and the Provider interface that every substrate implements.
//
// It is a leaf. It imports nothing else in this module, so that no other package
// can create an import cycle through it.
package models

import (
	"net/netip"
	"time"
)

// Sandbox is the record shard keeps for one sandbox. It is the whole of what
// shard inspect prints and what the state repository writes to disk.
//
// It never holds a secret value. A sandbox references a secret by name.
type Sandbox struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	Provider string `json:"provider"`
	State    State  `json:"state"`

	// PID is the host process that supervises the sandbox, or 0 when it does not
	// run. Which process that is depends on the substrate.
	PID int `json:"pid"`
	// NetnsPath is the network namespace the sandbox runs in.
	NetnsPath string `json:"netns_path"`
	// IP is the sandbox address inside that namespace.
	IP netip.Addr `json:"ip"`
	// HostInterface is the host end of the sandbox link, a veth or a tap. The
	// egress layer writes netfilter rules against this name.
	HostInterface string `json:"host_interface"`

	CreatedAt time.Time `json:"created_at"`
}
