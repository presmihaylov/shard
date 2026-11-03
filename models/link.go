package models

import "net/netip"

// LinkSubnet is where link addresses come from: below the 10.87.0.0/16 bridge and clear of the Docker, podman, Kubernetes and Tailscale defaults.
var LinkSubnet = netip.MustParsePrefix("10.86.0.0/16")

// Link is one sandbox another can reach by name: every source that links one target sees it at the same address.
type Link struct {
	Target string `json:"target" doc:"The sandbox the link reaches, by its name, or by its id when it has none."`
	// TargetID ties the link to the sandbox itself, so the remove of the target drops it.
	TargetID string     `json:"target_id" doc:"The id of the sandbox the link reaches."`
	Address  netip.Addr `json:"address" doc:"The address the source reaches the target on, from 10.86.0.0/16 and shared by every link to that target."`
}
