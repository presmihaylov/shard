package api

import (
	"slices"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
)

// Sandbox is the record a public route answers. It is built field by field, so a field the record gains stays off the wire until it is named here.
type Sandbox struct {
	ID            string             `json:"id"`
	Name          string             `json:"name,omitempty"`
	Image         string             `json:"image"`
	Digest        string             `json:"digest,omitempty"`
	Snapshot      string             `json:"snapshot,omitempty"`
	ForkedFrom    string             `json:"forked_from,omitempty"`
	Provider      string             `json:"provider"`
	Kernel        string             `json:"kernel,omitempty"`
	State         models.State       `json:"state" enum:"pending,created,running,paused,unresponsive,stopped,failed"`
	ExitStatus    *models.ExitStatus `json:"exit_status,omitempty"`
	StoppedReason string             `json:"stopped_reason,omitempty"`
	FailedReason  string             `json:"failed_reason,omitempty"`
	Resources     models.Resources   `json:"resources"`
	Command       []string           `json:"command,omitempty"`
	Restart       *models.Restart    `json:"restart,omitempty"`
	Secrets       []string           `json:"secrets,omitempty"`
	Policy        string             `json:"policy,omitempty"`
	StartedAt     time.Time          `json:"started_at,omitzero"`
	CreatedAt     time.Time          `json:"created_at"`
}

// Inspection is the public record beside the egress rules the host enforces for it.
type Inspection struct {
	Sandbox
	Egress *egress.Effective `json:"egress,omitempty"`
}

// Event is one step of the pull a create runs, less the host path the image lands at.
type Event struct {
	Status    string `json:"status" enum:"cached,pulling,layer,unpacking,unpacked,building,pulled" doc:"cached and pulled carry reference and digest; pulling adds layers and bytes, the whole download; layer carries one layer's digest, bytes and present; unpacking carries reference, digest and layers; unpacked carries one layer's digest, layer and layers; building carries nothing more."`
	Reference string `json:"reference,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Layers    int    `json:"layers,omitempty"`
	Layer     int    `json:"layer,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Present   bool   `json:"present,omitempty"`
}

// PublicSandbox leaves out the host side: the pid, the netns, the veth, the address, the checkpoint path and the daemon's own diagnosis.
func PublicSandbox(sb models.Sandbox) Sandbox {
	return Sandbox{
		ID:            sb.ID,
		Name:          sb.Name,
		Image:         sb.Image,
		Digest:        sb.Digest,
		Snapshot:      sb.Snapshot,
		ForkedFrom:    sb.ForkedFrom,
		Provider:      sb.Provider,
		Kernel:        sb.Kernel,
		State:         sb.State,
		ExitStatus:    sb.ExitStatus,
		StoppedReason: publicStoppedReason(sb.StoppedReason),
		FailedReason:  sandbox.PublicReason(sb),
		Resources:     sb.Resources,
		Command:       sb.Command,
		Restart:       sb.Restart,
		Secrets:       sb.Secrets,
		Policy:        sb.Policy,
		StartedAt:     sb.StartedAt,
		CreatedAt:     sb.CreatedAt,
	}
}

// publicStoppedReason permits only fixed diagnoses, so an old or new raw cause never reaches the wire.
func publicStoppedReason(reason string) string {
	switch reason {
	case "", sandbox.OOMKilledReason, sandbox.DiedReason, sandbox.LostReason:
		return reason
	}
	if strings.HasPrefix(reason, sandbox.SupervisorFailedReason+":") && strings.Contains(reason, models.ErrLostState.Error()) {
		return "the sandbox lost its lifecycle state; start it again"
	}
	if reason == sandbox.SupervisorFailedReason || strings.HasPrefix(reason, sandbox.SupervisorFailedReason+":") {
		return "the init process of the sandbox failed; remove it and create another sandbox"
	}

	return "the sandbox stopped; the daemon log has the cause"
}

func PublicInspection(insp sandbox.Inspection) Inspection {
	return Inspection{Sandbox: PublicSandbox(insp.Sandbox), Egress: publicEgress(insp.Egress)}
}

// publicEgress names what an implied rule opens as the dns group, never the bridge gateway shard's resolver sits on; ids keep their place, so an egress log still names the same rule.
func publicEgress(e *egress.Effective) *egress.Effective {
	if e == nil {
		return nil
	}

	out := *e
	out.Rules = listOf(slices.Clone(e.Rules))
	for i := range out.Rules {
		if out.Rules[i].Implied != "" {
			out.Rules[i].Destination = models.Destination{Kind: models.DestinationGroup, Value: egress.GroupDNS}
		}
	}

	return &out
}

func publicEvent(e image.Event) Event {
	return Event{Status: e.Status, Reference: e.Reference, Digest: e.Digest, Layers: e.Layers, Layer: e.Layer, Bytes: e.Bytes, Present: e.Present}
}
