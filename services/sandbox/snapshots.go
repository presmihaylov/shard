package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// SnapshotRequest names the stopped sandbox a snapshot copies. It is the JSON body of POST /v0/snapshots.
type SnapshotRequest struct {
	Sandbox string `json:"sandbox" minLength:"1"`
	Name    string `json:"name,omitempty"`
}

// snapshotLock keys a snapshot in the sandbox lock table; an id never holds a slash, so it never meets a sandbox id.
func snapshotLock(id string) string { return "snapshot/" + id }

// CreateSnapshot copies the files a stopped sandbox kept into a snapshot that outlives it.
func (s *Service) CreateSnapshot(ctx context.Context, req SnapshotRequest) (models.Snapshot, error) {
	if req.Sandbox == "" {
		return models.Snapshot{}, &RequestError{Err: errors.New("the request names no sandbox")}
	}
	if req.Name != "" {
		if err := sandboxstate.ValidSnapshotName(req.Name); err != nil {
			return models.Snapshot{}, err
		}
	}

	id, err := s.cfg.Repo.Resolve(req.Sandbox)
	if err != nil {
		return models.Snapshot{}, err
	}

	// A start under the copy would write the layer it reads.
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return models.Snapshot{}, err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Snapshot{}, err
	}
	if err := FailedGuard(id, sb); err != nil {
		return models.Snapshot{}, err
	}
	if sb.State != models.StateStopped {
		return models.Snapshot{}, wrongState(id, sb, fmt.Sprintf("stop it first with shard stop %s: a snapshot copies what a stop kept", id), models.CodeSandboxNotStopped)
	}

	img, found, err := s.cfg.Images.Lookup(sb.Image)
	if err != nil {
		return models.Snapshot{}, err
	}
	if !found {
		return models.Snapshot{}, &RequestError{Err: fmt.Errorf("sandbox %s was created from %s, which this host no longer holds, so a snapshot of it could never start: run shard pull %s first", id, sb.Image, sb.Image)}
	}
	// A create from the snapshot looks the tag up, so a tag that moved would mount the layer over another image.
	if img.Digest != sb.Digest {
		return models.Snapshot{}, &RequestError{Err: fmt.Errorf("sandbox %s was created from %s at %s, and this host now holds that tag at %s, so a snapshot of it could never start; snapshot a sandbox created from the current %s instead", id, sb.Image, sb.Digest, img.Digest, sb.Image)}
	}

	return s.cfg.Snapshots.Create(models.Snapshot{
		Name:       req.Name,
		Source:     id,
		SourceName: sb.Name,
		Image:      sb.Image,
		Digest:     sb.Digest,
		Provider:   s.cfg.Provider.Name(),
		DiskMiB:    sb.Resources.DiskMiB,
		MemoryMiB:  sb.Resources.MemoryMiB,
		CreatedAt:  time.Now().UTC(),
	}, func(files string) error { return s.cfg.Provider.Snapshot(ctx, id, files) })
}

// ListSnapshots answers every snapshot, ordered by id.
func (s *Service) ListSnapshots(_ context.Context) ([]models.Snapshot, error) {
	return s.cfg.Snapshots.List()
}

// InspectSnapshot answers one snapshot by its id or its name.
func (s *Service) InspectSnapshot(_ context.Context, ref string) (models.Snapshot, error) {
	id, err := s.cfg.Snapshots.Resolve(ref)
	if err != nil {
		return models.Snapshot{}, err
	}

	return s.cfg.Snapshots.Get(id)
}

// RemoveSnapshot deletes a snapshot. A sandbox made from it holds its own copy, so nothing refuses it.
func (s *Service) RemoveSnapshot(ctx context.Context, ref string) error {
	id, err := s.cfg.Snapshots.Resolve(ref)
	if err != nil {
		return err
	}

	// A create copying out of it holds this lock until its copy is done.
	unlock, err := s.lock(ctx, snapshotLock(id))
	if err != nil {
		return err
	}
	defer unlock()

	return s.cfg.Snapshots.Delete(id)
}

// seeded is a snapshot a create reads, held under its lock until unlock.
type seeded struct {
	req    CreateRequest
	id     string
	img    image.Image
	files  string
	unlock func()
}

// seed checks the snapshot a create names and fills the request from it: its image, and its disk and memory unless the request bounds them.
func (s *Service) seed(ctx context.Context, ref string, req CreateRequest) (seeded, error) {
	id, err := s.cfg.Snapshots.Resolve(ref)
	if err != nil {
		return seeded{}, err
	}

	unlock, err := s.lock(ctx, snapshotLock(id))
	if err != nil {
		return seeded{}, err
	}

	seed, err := s.readSeed(id, req)
	if err != nil {
		unlock()

		return seeded{}, err
	}
	seed.unlock = unlock

	return seed, nil
}

func (s *Service) readSeed(id string, req CreateRequest) (seeded, error) {
	snap, err := s.cfg.Snapshots.Get(id)
	if err != nil {
		return seeded{}, err
	}

	// The layer holds files only the substrate that wrote it knows how to mount.
	if snap.Provider != s.cfg.Provider.Name() {
		return seeded{}, &RequestError{Err: fmt.Errorf("snapshot %s was made on provider %s, and this server runs %s; create from it on a server that runs %s", id, snap.Provider, s.cfg.Provider.Name(), snap.Provider)}
	}

	// A create from a snapshot never pulls: a pull could bring another image than the one the layer sits over.
	img, found, err := s.cfg.Images.Lookup(snap.Image)
	if err != nil {
		return seeded{}, err
	}
	if !found {
		return seeded{}, &RequestError{Err: fmt.Errorf("snapshot %s was taken from %s at %s, which this host no longer holds, and a create from a snapshot never pulls; pull %s again if its tag still names %s, then retry, or create a new snapshot", id, snap.Image, snap.Digest, snap.Image, snap.Digest)}
	}
	if img.Digest != snap.Digest {
		return seeded{}, &RequestError{Err: fmt.Errorf("snapshot %s was taken from %s at %s, and this host now holds that tag at %s, and the snapshot fits only the image it was taken from; create a new snapshot from a sandbox created from the current %s", id, snap.Image, snap.Digest, img.Digest, snap.Image)}
	}

	if req.Resources.DiskMiB == 0 {
		req.Resources.DiskMiB = snap.DiskMiB
	}
	if req.Resources.MemoryMiB == nil {
		req.Resources.MemoryMiB = new(snap.MemoryMiB)
	}
	// A microVM substrate grows the copy of the disk file, and a shrink could cut off blocks the snapshot's files sit on.
	_, grows := s.cfg.Provider.(diskAdmitter)
	if grows && req.Resources.DiskMiB < snap.DiskMiB {
		return seeded{}, &RequestError{Err: fmt.Errorf("resources.disk_mib is %d MiB, smaller than the %d MiB disk of snapshot %s, and a disk only grows; omit it or set it to %d MiB or more", req.Resources.DiskMiB, snap.DiskMiB, id, snap.DiskMiB)}
	}
	// Every other substrate copies the files onto a new disk, which fails late inside the copy when too small; the source's own bound held them.
	bound, need := bundle.DiskBound(req.Resources.bounds()), (snap.Size+1<<20-1)>>20
	if !grows && bound < snap.DiskMiB && bound < need {
		return seeded{}, &RequestError{Err: fmt.Errorf("resources.disk_mib is %d MiB, smaller than the %d MiB the files of snapshot %s take; omit it or set it to %d MiB or more", req.Resources.DiskMiB, need, id, need)}
	}

	files, err := s.cfg.Snapshots.Files(id)
	if err != nil {
		return seeded{}, err
	}

	req.Image = snap.Image

	return seeded{req: req, id: id, img: img, files: files}, nil
}
