package cli

import "github.com/presmihaylov/shard/pkg/vzshim"

// shimState says whether this binary carries the VM shim, which only make build-darwin puts there.
func shimState() string {
	if vzshim.Embedded() {
		return "embedded"
	}

	return "absent"
}

func shimLine() string {
	if shimState() == "embedded" {
		return "vz shim embedded"
	}

	return "vz shim absent: run make build-darwin"
}
