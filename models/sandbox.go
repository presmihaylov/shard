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
	Name  string `json:"name,omitempty"`
	Image string `json:"image"`
	// Digest is the image the writable layer sits over, which a snapshot records: the tag in Image can move.
	Digest string `json:"digest,omitempty"`
	// Snapshot is the id of the snapshot the sandbox was created from, empty for one made from an image.
	Snapshot string `json:"snapshot,omitempty"`
	Provider string `json:"provider"`
	// Kernel is the guest kernel a microVM substrate booted, as its release tag; empty on a container substrate.
	Kernel string `json:"kernel,omitempty"`
	State  State  `json:"state"`
	// ExitStatus is the last entrypoint exit, nil until one happens. A sandbox has none of its own.
	ExitStatus *ExitStatus `json:"exit_status,omitempty"`
	// ExitChannel says why the daemon no longer reads the entrypoint exit from the guest, empty while it does.
	ExitChannel string `json:"exit_channel,omitempty"`
	// StoppedReason says why shard stopped it when no operator did, or why shard-init died on a stop; empty otherwise.
	StoppedReason string `json:"stopped_reason,omitempty"`
	// FailedReason says why a create never reached running or a pause lost the guest, set only in state failed.
	FailedReason string `json:"failed_reason,omitempty"`
	// UnresponsiveReason says what missed its probe bound, set only in state unresponsive.
	UnresponsiveReason string `json:"unresponsive_reason,omitempty"`

	// A resume keeps the checkpoint until the next pause or removal.
	Checkpoint string `json:"checkpoint,omitempty"`
	// Pausing is set for one pause, after it removed the old checkpoint, so any checkpoint found under it is that pause's own.
	Pausing bool `json:"pausing,omitempty"`

	// PID is the sandbox process on the host, or 0 when it does not run.
	PID       int          `json:"pid"`
	NetnsPath string       `json:"netns_path"`
	Address   netip.Prefix `json:"address"`
	// HostInterface is the host end of the veth. Netfilter rules target it.
	HostInterface string `json:"host_interface"`

	// Resources is what the sandbox was bounded by, because SHARD-24 start re-creates it from the record.
	Resources Resources `json:"resources"`

	// Command is the app shard run started under shard-init, empty for a sandbox create made.
	Command []string `json:"command,omitempty"`
	// Restart is the policy shard-init starts the entrypoint again under, nil for a sandbox without one.
	Restart *Restart `json:"restart,omitempty"`

	// Secrets names what the guest holds a placeholder for. The values live in the secret store and
	// reach a request only at the proxy, so this list is a grant and never a value.
	Secrets []string `json:"secrets,omitempty"`

	// Policy names the egress policy the host enforces for this sandbox, empty for none: then it may reach
	// the internet and nothing private.
	Policy string `json:"policy,omitempty"`

	// StartedAt is when the daemon last started the sandbox: ls reads its uptime, and liveness tells one run from the next.
	StartedAt time.Time `json:"started_at,omitzero"`
	CreatedAt time.Time `json:"created_at"`
}
