package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/pkg/store"
)

// configPath is the runtime spec of a built bundle, which is the only record of the guest environment.
func (b Bundle) configPath() string { return filepath.Join(b.Dir, "config.json") }

// CanSetEnv answers what SetEnv would refuse and writes nothing, so a grant can check before it plants.
func (b Bundle) CanSetEnv(name string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}

	return settable(spec.Process.Env, name)
}

// SetEnv adds one variable to the guest environment of a built bundle, which the next start reads.
func (b Bundle) SetEnv(name, value string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}

	if err := settable(spec.Process.Env, name); err != nil {
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

	env := slices.DeleteFunc(slices.Clone(spec.Process.Env), func(entry string) bool { return envKey(entry) == name })
	if len(env) == len(spec.Process.Env) {
		return nil
	}
	spec.Process.Env = env

	return b.writeSpec(spec)
}

// settable is the one predicate CanSetEnv and SetEnv share, so the check and the write cannot drift.
func settable(env []string, name string) error {
	if name == "" {
		return errors.New("the environment variable has no name")
	}
	if strings.ContainsAny(name, "=\x00") {
		return fmt.Errorf("%q is not an environment variable name", name)
	}

	for _, entry := range env {
		if envKey(entry) == name {
			return fmt.Errorf("the guest environment already holds %s, so nothing may be set over it", name)
		}
	}

	return nil
}

func envKey(entry string) string {
	key, _, _ := strings.Cut(entry, "=")

	return key
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
