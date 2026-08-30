// Package cli defines the shard commands and parses their flags.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
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
  shard create [flags] <image> [-- <argv>...]
                           create a sandbox, start its entrypoint and print its id
  shard exec [flags] <id|name> -- <argv>...
                           run a command in a sandbox that is already running
  shard start <id|name>    run a stopped sandbox again, over everything it kept
  shard pause <id|name>    write a snapshot of a running sandbox and free its memory
  shard resume <id|name>   run a paused sandbox again from its snapshot
  shard fork [--name <name>] <id|name>
                           start a new sandbox from the snapshot of another
  shard clone [--name <name>] <id|name>
                           start a new sandbox over a copy of the files a stopped one kept
  shard stop [flags] <id|name>
                           end a sandbox and keep everything it holds
  shard rm [flags] <id|name>
                           free what a stopped sandbox still holds
  shard ls [--all]         list the sandboxes that are up, and with --all the stopped ones too
  shard inspect <id|name>  print the record of a sandbox as JSON
  shard logs [-f] <id|name>
                           print what the entrypoint wrote, and with -f keep printing until it stops
  shard pull <image>       pull an image and unpack its rootfs
  shard image ls           list the pulled images
  shard image rm [--force] <image>
                           remove a pulled image, and with --force one a sandbox still references
  shard image prune        remove every pulled image no sandbox references
  shard version            print the version

Create flags, which must precede the image:
  --name <name>            a handle every verb takes in place of the id
  --env KEY=VALUE          set an environment variable, repeatable
  --workdir <dir>          the directory the entrypoint starts in
  --user <user>            the user the entrypoint runs as
  --memory <MiB>           the memory bound, 0 for unbounded
  --cpus <n>               the vcpu bound, 0 for unbounded

Exec flags, which must precede the id or name:
  -i                       keep stdin open on the command
  -t                       run the command on a terminal, and -it for both
  --env KEY=VALUE          set an environment variable, repeatable
  --workdir <dir>          the directory the command starts in
  --user <user>            the user the command runs as

Stop flags, which must precede the id or name:
  --time <duration>        how long the entrypoint gets before it is killed (default 10s)

Rm flags, which must precede the id or name:
  --force                  stop the sandbox first if it is still up
  --time <duration>        how long --force gives the entrypoint before it is killed

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
	// InitPath is the host path of the guest supervisor. It defaults to the environment when empty.
	InitPath string

	// newDeps builds the layers the commands drive. A test replaces it: the real parts need Linux and root.
	newDeps func(a App) *deps
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
	case "create":
		return a.create(ctx, args[1:])
	case "exec":
		return a.exec(ctx, args[1:])
	case "ls":
		return a.ls(ctx, args[1:])
	case "inspect":
		return a.inspect(ctx, args[1:])
	case "logs":
		return a.logs(ctx, args[1:])
	case "start":
		return a.start(ctx, args[1:])
	case "pause":
		return a.pause(ctx, args[1:])
	case "resume":
		return a.resume(ctx, args[1:])
	case "fork":
		return a.fork(ctx, args[1:])
	case "clone":
		return a.clone(ctx, args[1:])
	case "stop":
		return a.stop(ctx, args[1:])
	case "rm":
		return a.remove(ctx, args[1:])
	case "pull":
		return a.pull(ctx, args[1:])
	case "image":
		return a.image(ctx, args[1:])
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
	if a.InitPath == "" {
		a.InitPath = initPathFromEnv()
	}

	flags := flag.NewFlagSet("shard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&a.Root, "root", a.Root, "where shard keeps its state")
	flags.DurationVar(&a.Timeout, "timeout", a.Timeout, "how long a pull may take")
	flags.Var((*hostList)(&a.Insecure), "insecure-registry", "allow plaintext http to this registry host")

	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("parse the flags: %w", err)
	}

	// The fallback is the flag default, so an explicit empty or relative --root still lands here.
	if !filepath.IsAbs(a.Root) {
		return nil, fmt.Errorf("--root must be an absolute path, got %q", a.Root)
	}

	return flags.Args(), nil
}

// initPathFromEnv resolves where the guest supervisor lives on this host.
func initPathFromEnv() string {
	if path := os.Getenv(InitPathEnv); path != "" {
		return path
	}

	return DefaultInitPath
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
