// Package cli defines the shard commands and parses their flags.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// DefaultRoot is where shard keeps everything on the box.
const DefaultRoot = "/var/lib/shard"

// DefaultTimeout bounds one pull. Without it a registry that accepts and stalls pins a root process.
const DefaultTimeout = 30 * time.Minute

const usage = `shard - a single-node sandbox manager (pre-alpha)

Usage:
  shard pull <image>       pull an image and unpack its rootfs
  shard image ls           list the pulled images
  shard image rm <image>   remove a pulled image
  shard version            print the version

Flags:
  --root <dir>             where shard keeps its state (default ` + DefaultRoot + `)
  --timeout <duration>     how long a pull may take (default 30m)
  --insecure-registry <host>
                           allow plaintext http to this registry host, repeatable`

// App is the wiring one shard process needs.
type App struct {
	Version string
	// Root defaults to DefaultRoot when empty.
	Root string
	Out  io.Writer
	// Err carries warnings that must not fail the command. It defaults to nowhere.
	Err io.Writer
	// Insecure lists the registry hosts shard may reach over plaintext http. Every other host is https.
	Insecure []string
	// Timeout defaults to DefaultTimeout when zero.
	Timeout time.Duration
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
	if a.Timeout == 0 {
		a.Timeout = DefaultTimeout
	}

	flags := flag.NewFlagSet("shard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&a.Root, "root", a.Root, "where shard keeps its state")
	flags.DurationVar(&a.Timeout, "timeout", a.Timeout, "how long a pull may take")
	flags.Var((*hostList)(&a.Insecure), "insecure-registry", "allow plaintext http to this registry host")

	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("parse the flags: %w", err)
	}

	// Only an absent --root falls back: an empty or relative one would put the state tree under the cwd.
	if a.Root == "" && !given(flags, "root") {
		a.Root = DefaultRoot
	}
	if !filepath.IsAbs(a.Root) {
		return nil, fmt.Errorf("--root must be an absolute path, got %q", a.Root)
	}

	return flags.Args(), nil
}

func given(flags *flag.FlagSet, name string) bool {
	found := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})

	return found
}

// hostList collects a repeatable flag, which the flag package has no built-in type for.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(value string) error {
	*h = append(*h, value)

	return nil
}

// warn reports something the operator should know that is not a reason to fail the command.
func (a App) warn(message string) {
	if a.Err == nil {
		return
	}

	fmt.Fprintln(a.Err, "shard: warning:", message)
}

func (a App) print(s string) error {
	if _, err := fmt.Fprintln(a.Out, s); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
