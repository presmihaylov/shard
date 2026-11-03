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
	// ForkedFrom is the id of the sandbox this one was forked from, empty for one that was not.
	ForkedFrom string `json:"forked_from,omitempty"`
	Provider   string `json:"provider"`
	// Kernel is the guest kernel a microVM substrate last booted, as its release tag or local-<sha12>; empty on a container substrate.
	Kernel string `json:"kernel,omitempty"`
	State  State  `json:"state"`
	// ExitChannel says why the daemon no longer reads the process table from the guest, empty while it does.
	ExitChannel string `json:"exit_channel,omitempty"`
	// StoppedReason says why shard stopped it when no operator did, or why shard-init died; empty otherwise.
	StoppedReason string `json:"stopped_reason,omitempty"`
	// FailedReason is the raw cause of a create that never reached running or a pause that lost the guest; never served on a public route.
	FailedReason string `json:"failed_reason,omitempty"`
	// FailedPublic is the part of FailedReason a public route may answer, empty on a record older than it.
	FailedPublic string `json:"failed_public,omitempty"`
	// UnresponsiveReason says what missed its probe bound, set only in state unresponsive.
	UnresponsiveReason string `json:"unresponsive_reason,omitempty"`
	// OOM is what the host's memory kills did to the sandbox, nil until the first one.
	OOM *OOM `json:"oom,omitempty"`

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

	// Processes are the named processes shard run started, with the status the daemon last read of each.
	Processes []Process `json:"processes,omitempty"`
	// StoppedByOperator says a shard stop ended the last run, which keeps unless-stopped processes down on a daemon start.
	StoppedByOperator bool `json:"stopped_by_operator,omitempty"`
	// LogStarts is each process log's size when its current run started, where an attach begins; never served.
	LogStarts map[string]int64 `json:"log_starts,omitempty"`

	// Secrets names what the guest holds a placeholder for. The values live in the secret store and
	// reach a request only at the proxy, so this list is a grant and never a value.
	Secrets []string `json:"secrets,omitempty"`

	// Policy names the egress policy the host enforces for this sandbox, empty for none: then it may reach
	// the internet and nothing private.
	Policy string `json:"policy,omitempty"`

	// Ports are the host ports the daemon forwards into this sandbox while it runs; a fork starts with none.
	Ports []PortForward `json:"ports,omitempty"`

	// StartedAt is when the daemon last started the sandbox, which a resume keeps: ls reads its uptime from it.
	StartedAt time.Time `json:"started_at,omitzero"`
	// RunStartedAt is when this run began, a resume included, so liveness tells one run from the next; never served.
	RunStartedAt time.Time `json:"run_started_at,omitzero"`
	CreatedAt    time.Time `json:"created_at"`
}
