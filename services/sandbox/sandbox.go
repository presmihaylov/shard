// Package sandbox holds the verbs the CLI and the API share, so neither carries the rules itself.
package sandbox

import (
	"slices"

	"github.com/presmihaylov/shard/models"
)

// Reader is the part of sandboxstate.Repository the read verbs drive.
type Reader interface {
	Get(id string) (models.Sandbox, error)
	Resolve(ref string) (string, error)
	List() ([]models.Sandbox, error)
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
