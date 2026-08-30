package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// layersDir is where a snapshot keeps the copy of the writable layers its memory image was taken over.
const layersDir = "layers"

// Export copies config.json and the writable layers into dir, so a fork restores over what the memory saw.
func (b Bundle) Export(dir string) error {
	source := filepath.Join(b.Dir, "config.json")
	blob, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read %s: %w", source, err)
	}
	if err := store.WriteFile(filepath.Join(dir, "config.json"), blob, 0o644); err != nil { // #nosec G306
		return fmt.Errorf("copy config.json into the snapshot: %w", err)
	}

	for name, layer := range b.layers() {
		if err := copyTree(layer, filepath.Join(dir, layersDir, name)); err != nil {
			return err
		}
	}

	return nil
}

// Clone lays out a new bundle from what Export wrote, and keeps everything runsc checks a restore against.
func (s *Service) Clone(snapshot string, spec models.SandboxSpec) (Bundle, error) {
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

	for name, layer := range b.layers() {
		if err := copyTree(filepath.Join(snapshot, layersDir, name), layer); err != nil {
			return Bundle{}, err
		}
	}

	// The fork has its own address and name, and the layer copy still holds the source's.
	if err := writeNetworkFiles(b, spec); err != nil {
		return Bundle{}, err
	}

	configPath := filepath.Join(snapshot, "config.json")
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
	cfg.Linux.Namespaces = namespaces(spec.Network.NetnsPath)
	// The same mounts by destination, type and options, which the restore checks, over this bundle's sources.
	cfg.Mounts = mounts(b.ShardDir, b.Tmp, s.initPath, resourcesOf(cfg.Linux))

	encoded, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return Bundle{}, fmt.Errorf("marshal the runtime spec: %w", err)
	}

	target := filepath.Join(b.Dir, "config.json")
	if err := store.WriteFile(target, encoded, 0o644); err != nil { // #nosec G306
		return Bundle{}, fmt.Errorf("write %s: %w", target, err)
	}

	return b, nil
}

// layers names what a snapshot carries. The overlay work directory is scratch and is never copied.
func (b Bundle) layers() map[string]string {
	return map[string]string{"upper": b.Upper, "tmp": b.Tmp, "shard": b.ShardDir}
}

// copyTree is cp -a, because a file walk would drop the whiteout nodes and trusted xattrs of an upper layer.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	cmd := exec.Command("cp", "-a", src+string(filepath.Separator)+".", dst) // #nosec G204
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy %s to %s: %w: %s", src, dst, err, out)
	}

	return nil
}
