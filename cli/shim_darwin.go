package cli

import "github.com/presmihaylov/shard/pkg/vzshim"

// shimLine says whether this binary carries the VM shim, which only make build-darwin puts there.
func shimLine() string {
	if vzshim.Embedded() {
		return "vz shim embedded"
	}

	return "vz shim absent: run make build-darwin"
}
