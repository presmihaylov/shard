package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/pkg/store"
)

// SetEnv writes one variable into config.json, for a grant to a sandbox whose guest is not live.
// The exact entry standing already is the outcome, so a retried grant lands.
func (b Bundle) SetEnv(name, value string) error {
	return b.editEnv(func(env []string) ([]string, error) {
		entry := name + "=" + value
		if slices.Contains(env, entry) {
			return env, nil
		}
		if slices.ContainsFunc(env, func(e string) bool { return envKey(e) == name }) {
			return nil, fmt.Errorf("the guest environment already holds %s: the placeholder would hide it or be hidden by it", name)
		}

		return append(env, entry), nil
	})
}

// RemoveEnv takes one variable out of config.json. A name it does not hold is already the outcome.
func (b Bundle) RemoveEnv(name string) error {
	return b.editEnv(func(env []string) ([]string, error) {
		return slices.DeleteFunc(env, func(e string) bool { return envKey(e) == name }), nil
	})
}

// TrustProxy hands an existing bundle the proxy CA, because a sandbox created unfronted never got
// one and a granted secret makes its next start fronted.
func (b Bundle) TrustProxy(ca []byte) error {
	rt, err := b.Runtime()
	if err != nil {
		return err
	}

	if err := plantProxyCA(b, rt.RootFS, rt.Env, ca); err != nil {
		return err
	}

	return b.editEnv(func(env []string) ([]string, error) { return environment(env, true), nil })
}

func envKey(entry string) string {
	key, _, _ := strings.Cut(entry, "=")

	return key
}

func (b Bundle) editEnv(edit func([]string) ([]string, error)) error {
	configPath := filepath.Join(b.Dir, "config.json")

	blob, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(blob, &spec); err != nil {
		return fmt.Errorf("decode %s: %w", configPath, err)
	}
	if spec.Process == nil {
		return fmt.Errorf("%s names no process, so nothing holds the environment", configPath)
	}

	env, err := edit(spec.Process.Env)
	if err != nil {
		return err
	}
	spec.Process.Env = env

	encoded, err := json.MarshalIndent(&spec, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal the runtime spec: %w", err)
	}

	if err := store.WriteFile(configPath, encoded, 0o644); err != nil { // #nosec G306
		return fmt.Errorf("write %s: %w", configPath, err)
	}

	return nil
}
