// Package cli defines the shard commands and parses their flags.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
)

// DefaultRoot is where shard keeps everything on the box.
const DefaultRoot = "/var/lib/shard"

const usage = `shard - a single-node sandbox manager (pre-alpha)

Usage:
  shard pull <image>       pull an image and unpack its rootfs
  shard image ls           list the pulled images
  shard image rm <image>   remove a pulled image
  shard version            print the version

Flags:
  --root <dir>             where shard keeps its state (default ` + DefaultRoot + `)`

// App is the wiring one shard process needs.
type App struct {
	Version string
	// Root defaults to DefaultRoot when empty.
	Root string
	Out  io.Writer
}

// Run dispatches one command. A nil error means the command printed what it had to print.
func (a App) Run(ctx context.Context, args []string) error {
	// These read as flags, so they are answered before the flag parser can reject them.
	if len(args) == 1 {
		switch args[0] {
		case "version", "--version":
			return a.print(a.Version)
		case "help", "--help", "-h":
			return a.print(usage)
		}
	}

	args, err := a.parseGlobals(args)
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return a.print(usage)
	}

	switch args[0] {
	case "version":
		return a.print(a.Version)
	case "pull":
		return a.pull(ctx, args[1:])
	case "image":
		return a.image(args[1:])
	case "help":
		return a.print(usage)
	}

	return fmt.Errorf("unknown command %q; run shard help", args[0])
}

// parseGlobals takes the flags that precede the command and returns what is left.
func (a *App) parseGlobals(args []string) ([]string, error) {
	if a.Root == "" {
		a.Root = DefaultRoot
	}

	flags := flag.NewFlagSet("shard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&a.Root, "root", a.Root, "where shard keeps its state")

	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("parse the flags: %w", err)
	}

	return flags.Args(), nil
}

func (a App) print(s string) error {
	if _, err := fmt.Fprintln(a.Out, s); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
