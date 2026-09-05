// Package sandbox holds the verbs the CLI and the API share, so neither carries the rules itself.
package sandbox

import (
	"slices"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
)

// Reader is the part of sandboxstate.Repository the read verbs drive.
type Reader interface {
	Get(id string) (models.Sandbox, error)
	Resolve(ref string) (string, error)
	List() ([]models.Sandbox, error)
}

// Enforcer is the part of egress.Service inspect reads: what the host enforces for one sandbox.
type Enforcer interface {
	Effective(sb models.Sandbox) (egress.Effective, error)
}

// Inspection is the record plus what the host enforces for it, which the record only names.
type Inspection struct {
	models.Sandbox
	Egress *egress.Effective `json:"egress,omitempty"`
}

// List is what ls prints, beside the error that names the records it could not read.
func List(repo Reader, all bool) ([]models.Sandbox, error) {
	sandboxes, unreadable := repo.List()

	// A stopped sandbox holds no process, so it is shown on all only.
	if !all {
		sandboxes = slices.DeleteFunc(sandboxes, func(sb models.Sandbox) bool { return sb.State == models.StateStopped })
	}

	return sandboxes, unreadable
}

// Get answers for an id or a name. A name nothing holds falls through to Get, which says ErrNotFound.
func Get(repo Reader, ref string) (models.Sandbox, error) {
	id, err := repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	return repo.Get(id)
}

// Inspect is what inspect prints: the record, and the egress rules when the record names a policy.
func Inspect(repo Reader, enforcer Enforcer, ref string) (Inspection, error) {
	sb, err := Get(repo, ref)
	if err != nil {
		return Inspection{}, err
	}

	out := Inspection{Sandbox: sb}
	if sb.Policy == "" {
		return out, nil
	}

	effective, err := enforcer.Effective(sb)
	if err != nil {
		return Inspection{}, err
	}
	out.Egress = &effective

	return out, nil
}
