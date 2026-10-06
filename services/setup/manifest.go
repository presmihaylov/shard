package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ManifestPath sits outside the data dir and is root-owned, since uninstall removes with root what it names.
const ManifestPath = "/var/lib/shard-setup/manifest.json"

// Kind says what uninstall does with an Owned file.
type Kind string

const (
	// KindBinary, KindService and KindConfig are shard's own; uninstall removes them.
	KindBinary  Kind = "binary"
	KindService Kind = "service"
	KindConfig  Kind = "config"
	// KindTool is a shared dependency setup installed; uninstall lists it and keeps it.
	KindTool Kind = "tool"
	// KindData is saved data; uninstall keeps it.
	KindData Kind = "data"
)

// Owned is one file setup created. Path is on the host, without Host.Root.
type Owned struct {
	Path string `json:"path"`
	Kind Kind   `json:"kind"`
	// Package is the system package a tool came from, so uninstall can name the command that removes it.
	Package string `json:"package,omitempty"`
}

// Manifest is what setup installed, in the order it did it.
type Manifest struct {
	Version     string `json:"version"`
	Provider    string `json:"provider"`
	StartAtBoot bool   `json:"start_at_boot"`
	StorageMiB  int64  `json:"storage_mib,omitempty"`
	// API is the address shard serve listens on, empty when setup configured no HTTP API.
	API   string  `json:"http_api,omitempty"`
	Files []Owned `json:"files"`
}

// RecordOwned adds files to the manifest after setup created them; a file that was already there is never recorded.
func RecordOwned(ctx context.Context, h Host, local Local, files ...Owned) error {
	return record(ctx, h, func(m *Manifest) {
		m.Version, m.Provider, m.StartAtBoot, m.StorageMiB, m.API = h.Version, local.Provider, local.StartAtBoot, local.StorageMiB, local.API
	}, files...)
}

func record(ctx context.Context, h Host, set func(*Manifest), files ...Owned) error {
	m, _, err := LoadManifest(h)
	if err != nil {
		return err
	}

	set(&m)
	for _, f := range files {
		if err := f.valid(); err != nil {
			return err
		}
		i := slices.IndexFunc(m.Files, func(o Owned) bool { return o.Path == f.Path })
		if i >= 0 {
			m.Files[i] = f
			continue
		}
		m.Files = append(m.Files, f)
	}

	return saveManifest(ctx, h, m)
}

// LoadManifest reads the manifest, and reports false when setup never installed on this host.
func LoadManifest(h Host) (Manifest, bool, error) {
	path := filepath.Join(h.Root, ManifestPath)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, fmt.Errorf("read the installation manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, false, fmt.Errorf("decode the installation manifest %s: %w", path, err)
	}
	for _, f := range m.Files {
		if err := f.valid(); err != nil {
			return Manifest{}, false, fmt.Errorf("installation manifest %s: %w", path, err)
		}
	}

	return m, true, nil
}

// saveManifest writes as the user, then moves the file into the root-owned dir with one rename.
func saveManifest(ctx context.Context, h Host, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the installation manifest: %w", err)
	}

	tmp, err := os.CreateTemp("", "shard-manifest-*.json")
	if err != nil {
		return fmt.Errorf("write the installation manifest: %w", err)
	}
	_, err = tmp.Write(append(data, '\n'))
	if err := errors.Join(err, tmp.Close()); err != nil {
		return errors.Join(fmt.Errorf("write the installation manifest: %w", err), os.Remove(tmp.Name()))
	}

	path := filepath.Join(h.Root, ManifestPath)
	next := path + ".new"
	steps := [][]string{
		{"mkdir", "-p", "-m", "0755", filepath.Dir(path)},
		{"install", "-m", "0644", tmp.Name(), next},
		{"mv", "-f", next, path},
	}
	for _, step := range steps {
		if _, err := privileged(ctx, h, step[0], step[1:]...); err != nil {
			return errors.Join(fmt.Errorf("save the installation manifest: %w", err), os.Remove(tmp.Name()))
		}
	}

	if err := os.Remove(tmp.Name()); err != nil {
		return fmt.Errorf("save the installation manifest: %w", err)
	}

	return nil
}

// removeManifest is the last step of an uninstall, so a failed one can run again from the same list.
func removeManifest(ctx context.Context, h Host) error {
	path := filepath.Join(h.Root, ManifestPath)
	if _, err := privileged(ctx, h, "rm", "-f", path); err != nil {
		return fmt.Errorf("remove the installation manifest: %w", err)
	}
	if _, err := privileged(ctx, h, "rmdir", filepath.Dir(path)); err != nil {
		return fmt.Errorf("remove %s: %w", filepath.Dir(path), err)
	}

	return nil
}

// valid keeps a manifest from naming a relative or unclean path, which a root rm would resolve somewhere else.
func (f Owned) valid() error {
	if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path || f.Path == "/" {
		return fmt.Errorf("owned file %q is not a clean absolute path", f.Path)
	}
	switch f.Kind {
	case KindBinary, KindService, KindConfig, KindTool, KindData:
		return nil
	}

	return fmt.Errorf("owned file %s has unknown kind %q", f.Path, f.Kind)
}
