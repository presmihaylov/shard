// Package models holds the domain types of shard: the sandbox, its states and the Provider.
package models

import (
	"net/netip"
	"time"
)

// Sandbox is the record shard keeps for one sandbox. It never holds a secret value.
type Sandbox struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	Provider string `json:"provider"`
	State    State  `json:"state"`
	// ExitStatus is the last entrypoint exit, nil until one happens. A sandbox has none of its own.
	ExitStatus *ExitStatus `json:"exit_status,omitempty"`

	// PID is the sandbox process on the host, or 0 when it does not run.
	PID       int          `json:"pid"`
	NetnsPath string       `json:"netns_path"`
	Address   netip.Prefix `json:"address"`
	// HostInterface is the host end of the link, a veth or a tap. Netfilter rules target it.
	HostInterface string `json:"host_interface"`

	CreatedAt time.Time `json:"created_at"`
}
