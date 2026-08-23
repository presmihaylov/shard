package models

import (
	"slices"
	"strings"
)

// Resolve fills the spec from the image config it will run over. Every provider needs the same
// precedence, so it lives here rather than being redone once per substrate.
func (s SandboxSpec) Resolve(cfg ImageConfig) SandboxSpec {
	if len(s.Entrypoint) == 0 {
		s.Entrypoint = slices.Concat(cfg.Entrypoint, cfg.Cmd)
	}

	s.Env = mergeEnv(cfg.Env, s.Env)
	s.WorkDir = firstNonEmpty(s.WorkDir, cfg.WorkDir)
	s.User = firstNonEmpty(s.User, cfg.User)
	// The guest hostname comes from the name, and every sandbox has one so a later process rebuilds it.
	s.Name = firstNonEmpty(s.Name, s.ID)

	return s
}

// mergeEnv keeps the image order, so an image that sets a variable twice still resolves the same way.
func mergeEnv(imageEnv, overrides []string) []string {
	env := make([]string, 0, len(imageEnv)+len(overrides))
	applied := make(map[string]bool, len(overrides))

	byKey := make(map[string]string, len(overrides))
	for _, entry := range overrides {
		if key, _, found := strings.Cut(entry, "="); found {
			byKey[key] = entry
		}
	}

	for _, entry := range imageEnv {
		key, _, found := strings.Cut(entry, "=")
		// An entry with no "=" is not an assignment, so no runtime would accept it.
		if !found {
			continue
		}

		override, ok := byKey[key]
		if !ok {
			env = append(env, entry)

			continue
		}

		env = append(env, override)
		applied[key] = true
	}

	for _, entry := range overrides {
		key, _, found := strings.Cut(entry, "=")
		if !found || applied[key] {
			continue
		}

		// byKey, not entry: a spec that sets one key twice must resolve the same way in both loops.
		env = append(env, byKey[key])
		applied[key] = true
	}

	return env
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
