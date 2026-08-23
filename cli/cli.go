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
  shard run [flags] <image> [-- <argv>...]
                           run a sandbox in the foreground and stream its output
  shard pull <image>       pull an image and unpack its rootfs
  shard image ls           list the pulled images
  shard image rm <image>   remove a pulled image
  shard version            print the version

Run flags, which must precede the image:
  --env KEY=VALUE          set an environment variable, repeatable
  --workdir <dir>          the directory the entrypoint starts in
  --user <user>            the user the entrypoint runs as
  --memory <MiB>           the memory bound, 0 for unbounded
  --cpus <n>               the vcpu bound, 0 for unbounded
  --shard-init <path>      the host path of the guest supervisor

The guest's stdout and stderr are interleaved into one stream, so shard run cannot
separate them. The sandbox outlives the command: run prints its id and Ctrl-C only
detaches from it.

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

	// newRunDeps builds what run wires together. A test replaces it: every real part needs Linux and root.
	newRunDeps func(a App, opts runOptions) (runDeps, error)
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
	case "run":
		return a.run(ctx, args[1:])
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
	if a.Root == "" {
		a.Root = DefaultRoot
	}

	flags := flag.NewFlagSet("shard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&a.Root, "root", a.Root, "where shard keeps its state")
	flags.DurationVar(&a.Timeout, "timeout", a.Timeout, "how long a pull may take")
	flags.Var((*stringList)(&a.Insecure), "insecure-registry", "allow plaintext http to this registry host")

	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("parse the flags: %w", err)
	}

	// The fallback is the flag default, so an explicit empty or relative --root still lands here.
	if !filepath.IsAbs(a.Root) {
		return nil, fmt.Errorf("--root must be an absolute path, got %q", a.Root)
	}

	return flags.Args(), nil
}

// stringList collects a repeatable flag, which the flag package has no built-in type for.
type stringList []string

func (h *stringList) String() string { return strings.Join(*h, ",") }

func (h *stringList) Set(value string) error {
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

// note tells the operator something they need and that is not a warning, such as the sandbox handle.
func (a App) note(message string) {
	if a.Err == nil {
		return
	}

	fmt.Fprintln(a.Err, "shard:", message)
}

func (a App) print(s string) error {
	if _, err := fmt.Fprintln(a.Out, s); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
