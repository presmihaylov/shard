// Package models holds the domain types of shard: the sandbox, its states and the Provider.
package models

import (
	"net/netip"
	"time"
)

// Sandbox is the record shard keeps for one sandbox. It never holds a secret value.
type Sandbox struct {
	// ID is generated and human readable. Every verb takes it, and takes Name in its place.
	ID string `json:"id"`
	// Name is the handle --name gave it, empty when none. The guest hostname is this, or the id when
	// this is empty.
	Name     string `json:"name,omitempty"`
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

	// Resources is what the sandbox was bounded by, because SHARD-24 start re-creates it from the record.
	Resources Resources `json:"resources"`

	CreatedAt time.Time `json:"created_at"`
}
