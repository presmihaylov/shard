package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// layersDir is where a checkpoint keeps the copy of the writable layers its memory image was taken over.
const layersDir = "layers"

// seedLayers is what a snapshot keeps: /.shard and the logs hold the last run's ready file and its processes' output, which a new sandbox must not inherit.
var seedLayers = []string{"upper", "tmp"}

// Export copies config.json and the writable layers into dir, so a fork restores over what the memory saw; ctx ends the copy, which runs while the guest is frozen.
func (b Bundle) Export(ctx context.Context, dir string) error {
	source := filepath.Join(b.Dir, "config.json")
	blob, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read %s: %w", source, err)
	}
	if err := store.WriteFile(filepath.Join(dir, "config.json"), blob, 0o644); err != nil { // #nosec G306
		return fmt.Errorf("copy config.json into the checkpoint: %w", err)
	}

	for name, layer := range b.layers() {
		if err := copyTree(ctx, layer, filepath.Join(dir, layersDir, name)); err != nil {
			return err
		}
	}

	// The process table lives off the layers, so a fork must carry it, or the fork would answer for no process until its restored shard-init reports again.
	if err := copyExitFile(b.ExitFile, filepath.Join(dir, exitFileName)); err != nil {
		return err
	}

	return nil
}

// Fork lays out a new bundle from what Export wrote, and keeps everything runsc checks a restore against.
func (s *Service) Fork(checkpoint string, spec models.SandboxSpec) (Bundle, error) {
	layers := map[string]string{}
	for name := range (Bundle{}).layers() {
		layers[name] = filepath.Join(checkpoint, layersDir, name)
	}

	// A fork carries the source's process table, so it answers for the processes it restores at once.
	return s.copyBundle(filepath.Join(checkpoint, "config.json"), layers, filepath.Join(checkpoint, exitFileName), spec)
}

// Snapshot copies the writable layer and /tmp of a stopped bundle into dir, which a Build reads back as its Seed.
func (b Bundle) Snapshot(ctx context.Context, dir string) error {
	// The layers sit on the disk the stop detached.
	return b.withDisk(func() error {
		// A source that was never built has no layer, and copyTree would create the copy before cp fails.
		for _, name := range seedLayers {
			if _, err := os.Stat(b.layers()[name]); err != nil {
				return fmt.Errorf("read the %s layer to snapshot: %w", name, err)
			}
		}
		for _, name := range seedLayers {
			if err := copyTree(ctx, b.layers()[name], filepath.Join(dir, name)); err != nil {
				return err
			}
		}

		return nil
	})
}

// seed fills a fresh bundle's layers from a snapshot, before Build writes this sandbox's own files over them.
func seed(b Bundle, dir string) error {
	if dir == "" {
		return nil
	}

	for _, name := range seedLayers {
		if err := copyTree(context.Background(), filepath.Join(dir, name), b.layers()[name]); err != nil {
			return err
		}
	}

	return nil
}

// copyBundle copies the layers and rewrites config.json under the new identity, and carries the source's process table.
func (s *Service) copyBundle(configPath string, layers map[string]string, sourceExit string, spec models.SandboxSpec) (Bundle, error) {
	if spec.ID == "" || spec.StateDir == "" {
		return Bundle{}, fmt.Errorf("a fork needs an id and a state directory, got %q and %q", spec.ID, spec.StateDir)
	}

	b, err := newBundle(spec.StateDir)
	if err != nil {
		return Bundle{}, err
	}

	if err := layout(b); err != nil {
		return Bundle{}, err
	}

	// No guest waits frozen on this copy, so nothing cuts it short.
	for name, layer := range b.layers() {
		if err := copyTree(context.Background(), layers[name], layer); err != nil {
			return Bundle{}, err
		}
	}

	// The fork has its own address and name, and the layer copy still holds the source's.
	if err := writeNetworkFiles(b, spec); err != nil {
		return Bundle{}, err
	}

	blob, err := os.ReadFile(configPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("read %s: %w", configPath, err)
	}

	var cfg specs.Spec
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return Bundle{}, fmt.Errorf("parse %s: %w", configPath, err)
	}
	if cfg.Linux == nil {
		return Bundle{}, fmt.Errorf("%s has no linux section", configPath)
	}

	cfg.Hostname = firstNonEmpty(spec.Name, spec.ID)
	cfg.Linux.CgroupsPath = CgroupsPath(spec.ID)
	cfg.Linux.Namespaces = namespaces(spec.Network)
	cfg.Linux.UIDMappings = idMappings(spec.Network.Userns)
	cfg.Linux.GIDMappings = idMappings(spec.Network.Userns)
	// The same mounts by destination, type and options, which a restore checks, over this bundle's sources.
	cfg.Mounts = mounts(b.ShardDir, b.Logs, b.Tmp, s.initPath, resourcesOf(cfg.Linux))

	encoded, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return Bundle{}, fmt.Errorf("marshal the runtime spec: %w", err)
	}

	target := filepath.Join(b.Dir, "config.json")
	if err := store.WriteFile(target, encoded, 0o644); err != nil { // #nosec G306
		return Bundle{}, fmt.Errorf("write %s: %w", target, err)
	}

	if err := copyExitFile(sourceExit, b.ExitFile); err != nil {
		return Bundle{}, err
	}

	return b, nil
}

// copyExitFile carries shard-init's process table between a bundle and a checkpoint; a missing source is not an error.
func copyExitFile(src, dst string) error {
	blob, err := readExitFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	if err := store.WriteFile(dst, blob, 0o600); err != nil {
		return fmt.Errorf("copy the process table to %s: %w", dst, err)
	}

	return nil
}

// layers names what a checkpoint carries; the logs go too, as a restore reopens each one at its path. The overlay work directory is scratch and is never copied.
func (b Bundle) layers() map[string]string {
	return map[string]string{"upper": b.Upper, "tmp": b.Tmp, "shard": b.ShardDir, "logs": b.Logs}
}

// copyTree is cp -a, because a file walk would drop the whiteout nodes and trusted xattrs of an upper layer.
func copyTree(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	cmd := exec.CommandContext(ctx, "cp", "-a", src+string(filepath.Separator)+".", dst) // #nosec G204
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy %s to %s: %w: %s", src, dst, err, out)
	}

	return nil
}
