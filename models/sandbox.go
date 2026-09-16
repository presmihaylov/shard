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
	// StoppedReason says why shard stopped it when no operator did, empty otherwise.
	StoppedReason string `json:"stopped_reason,omitempty"`

	// Snapshot is the directory the last pause wrote, empty until one happens. A resume reads it and
	// does not consume it, so it stands until the next pause replaces it or rm removes it.
	Snapshot string `json:"snapshot,omitempty"`

	// PID is the sandbox process on the host, or 0 when it does not run.
	PID       int          `json:"pid"`
	NetnsPath string       `json:"netns_path"`
	Address   netip.Prefix `json:"address"`
	// HostInterface is the host end of the link, a veth or a tap. Netfilter rules target it.
	HostInterface string `json:"host_interface"`

	// Resources is what the sandbox was bounded by, because SHARD-24 start re-creates it from the record.
	Resources Resources `json:"resources"`

	// RestartOnOOM asks the daemon to start the sandbox again when the host ends it for its memory.
	RestartOnOOM bool `json:"restart_on_oom,omitempty"`
	// OOMRestarts counts those starts, and OOMRestartedAt is the last one, which the next backoff counts from.
	OOMRestarts    int       `json:"oom_restarts,omitempty"`
	OOMRestartedAt time.Time `json:"oom_restarted_at,omitzero"`

	// HealthCheck is the probe the daemon runs while the sandbox runs, and Health what it found, both nil without one.
	HealthCheck *HealthCheck `json:"health_check,omitempty"`
	Health      *Health      `json:"health,omitempty"`

	// Restart is the policy shard-init starts the entrypoint again under, nil for a sandbox without one.
	Restart *Restart `json:"restart,omitempty"`

	// Secrets names what the guest holds a placeholder for. The values live in the secret store and
	// reach a request only at the proxy, so this list is a grant and never a value.
	Secrets []string `json:"secrets,omitempty"`

	// Policy names the egress policy the host enforces for this sandbox, empty for none: then it may reach
	// the internet and nothing private.
	Policy string `json:"policy,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}
