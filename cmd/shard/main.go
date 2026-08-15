// Command shard is a single-node sandbox manager.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time via -ldflags.
var version = "dev"

const usage = `shard - a single-node sandbox manager (pre-alpha, no verbs implemented yet)

Usage:
  shard version    print the version`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "shard:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		_, err := fmt.Fprintln(out, version)
		return err
	}

	_, err := fmt.Fprintln(out, usage)
	return err
}
