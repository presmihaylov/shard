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

	// PID supervises the sandbox on the host, or 0 when it does not run.
	PID       int        `json:"pid"`
	NetnsPath string     `json:"netns_path"`
	IP        netip.Addr `json:"ip"`
	// HostInterface is the host end of the link, a veth or a tap. Netfilter rules target it.
	HostInterface string `json:"host_interface"`

	CreatedAt time.Time `json:"created_at"`
}
