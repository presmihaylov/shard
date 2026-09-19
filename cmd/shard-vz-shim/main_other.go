//go:build !darwin

// shard-vz-shim holds one Virtualization.framework VM; off a Mac there is nothing to hold.
package main

import (
	"fmt"
	"os"

	"github.com/presmihaylov/shard/pkg/vz"
)

func main() {
	fmt.Fprintln(os.Stderr, "shard-vz-shim:", vz.ErrUnsupported)
	os.Exit(1)
}
