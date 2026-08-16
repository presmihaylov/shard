// Command shard is a single-node sandbox manager.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/presmihaylov/shard/cli"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	app := cli.App{Version: version, Out: os.Stdout}

	if err := app.Run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "shard:", err)
		os.Exit(1)
	}
}
