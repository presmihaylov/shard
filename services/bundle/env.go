package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/runspec"
)

// configPath is the runtime spec of a built bundle, which is the only record of the guest environment.
func (b Bundle) configPath() string { return filepath.Join(b.Dir, "config.json") }

// CanSetEnv answers what SetEnv would refuse and writes nothing, so a grant can check before it plants.
func (b Bundle) CanSetEnv(name string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}

	return runspec.Settable(spec.Process.Env, name)
}

// SetEnv adds one variable to the guest environment of a built bundle, which the next start reads.
func (b Bundle) SetEnv(name, value string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}

	if err := runspec.Settable(spec.Process.Env, name); err != nil {
		return err
	}

	spec.Process.Env = append(spec.Process.Env, name+"="+value)

	return b.writeSpec(spec)
}

// RemoveEnv drops every entry of that name. A bundle that holds none is the outcome asked for.
func (b Bundle) RemoveEnv(name string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}

	env := runspec.RemoveEnv(spec.Process.Env, name)
	if len(env) == len(spec.Process.Env) {
		return nil
	}
	spec.Process.Env = env

	return b.writeSpec(spec)
}

// readSpec reads config.json back. A bundle with no process names no environment, so it is refused.
func (b Bundle) readSpec() (*specs.Spec, error) {
	blob, err := os.ReadFile(b.configPath())
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", b.configPath(), err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(blob, &spec); err != nil {
		return nil, fmt.Errorf("decode %s: %w", b.configPath(), err)
	}

	if spec.Process == nil {
		return nil, fmt.Errorf("%s names no process, so nothing says what the entrypoint runs with", b.configPath())
	}

	return &spec, nil
}

func (b Bundle) writeSpec(spec *specs.Spec) error {
	encoded, err := json.MarshalIndent(spec, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal the runtime spec: %w", err)
	}

	if err := store.WriteFile(b.configPath(), encoded, 0o644); err != nil { // #nosec G306
		return fmt.Errorf("write %s: %w", b.configPath(), err)
	}

	return nil
}

// Opener answers the guest environment of any sandbox by its state dir, which on every OCI substrate is its bundle.
type Opener func(id string) (string, error)

func (o Opener) Environment(id string) (models.Environment, error) {
	dir, err := o(id)
	if err != nil {
		return nil, err
	}

	return Open(dir)
}
