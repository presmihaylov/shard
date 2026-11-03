package vzvm

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// Environment is the run message in the record, which the next start sends the guest; a VM has no bundle to edit.
func (p *Provider) Environment(id string) (models.Environment, error) {
	dir, _, err := p.open(id)
	if err != nil {
		return nil, err
	}

	return &environment{id: id, dir: dir}, nil
}

type environment struct {
	id, dir string
}

func (e *environment) CanSetEnv(name string) error {
	r, err := e.read()
	if err != nil {
		return err
	}

	return runspec.Settable(r.Run.Env, name)
}

func (e *environment) SetEnv(name, value string) error {
	return e.update(func(r *record) error {
		if err := runspec.Settable(r.Run.Env, name); err != nil {
			return err
		}
		r.Run.Env = append(r.Run.Env, name+"="+value)

		return nil
	})
}

func (e *environment) RemoveEnv(name string) error {
	return e.update(func(r *record) error {
		r.Run.Env = runspec.RemoveEnv(r.Run.Env, name)

		return nil
	})
}

func (e *environment) TrustProxy(proxyCA []byte) error {
	if len(proxyCA) == 0 {
		return errors.New("no proxy CA: there is nothing for the sandbox to trust")
	}

	return e.update(func(r *record) error { return r.trust(proxyCA) })
}

// Resize grows the disk before it rewrites the record, so a refused grow leaves the sandbox with the bounds it had.
func (e *environment) Resize(res models.Resources) error {
	r, err := e.read()
	if err != nil {
		return err
	}
	have, want := bundle.DiskBound(r.Resources), bundle.DiskBound(res)
	if want < have {
		return fmt.Errorf("sandbox %s: its %d MiB disk does not shrink to %d MiB", e.id, have, want)
	}
	if want > have {
		clone := func(from, to string) error {
			_, err := bundle.CloneFile(from, to)

			return err
		}
		if err := bundle.GrowDisk(filepath.Join(e.dir, diskFile), res, clone); err != nil {
			return fmt.Errorf("sandbox %s: %w", e.id, err)
		}
	}

	return e.update(func(r *record) error {
		r.Resources = res

		return nil
	})
}

func (e *environment) read() (record, error) {
	r, found, err := readRecord(e.dir)
	if err != nil {
		return record{}, err
	}
	if !found {
		return record{}, fmt.Errorf("sandbox %s does not exist on %s", e.id, Name)
	}

	return r, nil
}

// update rewrites the record whole, so a change the guest environment refuses leaves it as it was.
func (e *environment) update(change func(r *record) error) error {
	r, err := e.read()
	if err != nil {
		return err
	}
	if err := change(&r); err != nil {
		return fmt.Errorf("sandbox %s: %w", e.id, err)
	}

	return writeRecord(e.dir, r)
}
