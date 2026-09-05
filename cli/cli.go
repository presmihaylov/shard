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

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/serve"
)

// DefaultRoot is where shard keeps everything on the box.
const DefaultRoot = "/var/lib/shard"

// DefaultTimeout bounds one pull inside the daemon. Without it a registry that accepts and stalls pins it.
const DefaultTimeout = 30 * time.Minute

// DefaultInitPath is where make devbox-sync installs the guest supervisor on the box.
const DefaultInitPath = "/usr/local/bin/shard-init"

// InitPathEnv overrides DefaultInitPath. It is a property of the install, so it is no create flag.
const InitPathEnv = "SHARD_INIT_PATH"

// The environment behind the three flags that point a verb at a remote daemon, as docker's DOCKER_HOST does.
const (
	HostEnv      = "SHARD_HOST"
	TokenFileEnv = "SHARD_TOKEN_FILE" //nolint:gosec // G101: this names a file, and holds no token
	CAFileEnv    = "SHARD_CA_FILE"
)

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
                           start a new sandbox over a copy of the files a stopped or paused one kept
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
  shard secret set --to <host>... [--mock-value <v>] <NAME>
                           store a secret read from stdin, granted to those hosts; set again to rotate the value
  shard secret ls          list the secrets by name and destination, never by value
  shard secret rm [--force] <NAME>
                           remove a secret, and with --force one a sandbox still holds
  shard policy create [--allow <rule>]... [--deny <rule>]... <name>
                           store an egress policy, rules first match first, and drop what none match
  shard policy show <name> print a policy as JSON
  shard policy ls          list the policies
  shard policy rm <name>   remove a policy no sandbox holds
  shard daemon             run the resident process that owns the sandbox lifecycle, the background work and the API socket; systemd starts it
  shard serve [flags]      accept TLS on a TCP address, check the bearer token and pass the bytes to the daemon socket; its own unit starts it
  shard version            print the version of this binary and of the daemon; --version prints the first alone and never fails

A rule is <destination> [tcp|udp[:<ports>]], with ports as a comma list of numbers and ranges.
The destination is a host, an address or a prefix, or any:
  10.0.0.0/8 tcp:22   api.example.com   any udp:53
A name rule is tcp to ports 80 and 443 only, and both when no port is named.

Create flags, which must precede the image:
  --name <name>            a handle every verb takes in place of the id
  --env KEY=VALUE          set an environment variable, repeatable
  --secret <NAME>          hand the guest a placeholder for a stored secret as $NAME, repeatable
  --policy <name>          the egress policy the host enforces; without one the sandbox reaches the internet and nothing private
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

Serve flags:
  --listen <addr>          the address to accept on (default ` + serve.DefaultListen + `)
  --cert <pem>             the tls certificate to serve; without a pair serve refuses to start
  --key <pem>              the key of that certificate
  --token-file <path>      the file holding the bearer token every request carries

Flags:
  --root <dir>             where shard keeps its state (default ` + DefaultRoot + `)
  --timeout <duration>     how long a pull may take, read by the daemon (default 30m)
  --insecure-registry <host>
                           allow plaintext http to this registry host, repeatable
  --host <url>             speak to a shard serve front, as https://box:2376, instead of the socket
  --token-file <path>      the bearer token that front checks
  --ca-file <pem>          the certificate that signed the front's own

--host, --token-file and --ca-file also come from ` + HostEnv + `, ` + TokenFileEnv + ` and ` + CAFileEnv + `.`

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
	// Host is the shard serve front a verb speaks to instead of the socket, as https://box:2376.
	Host string
	// TokenFile holds the bearer token that front checks, and CAFile the certificate that signed its own.
	TokenFile string
	CAFile    string

	// remote is the client of Host, built once the globals are parsed and before any verb runs.
	remote *client.Client

	// clientTimeout bounds one daemon call. A test sets it; zero keeps the client's default.
	clientTimeout time.Duration

	// in is the terminal this shard process holds. A test replaces it: a pipe is not a terminal.
	in *os.File
}

// stdin is what exec hands the guest and what secret set reads the value from.
func (a App) stdin() *os.File {
	if a.in == nil {
		return os.Stdin
	}

	return a.in
}

// Run dispatches one command. A nil error means the command printed what it had to print.
func (a App) Run(ctx context.Context, args []string) error {
	// These read as flags, so they are answered before the flag parser can reject them.
	if len(args) == 1 {
		switch args[0] {
		case "--version":
			return a.print("client " + a.Version)
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
		return a.version(ctx)
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
	case "secret":
		return a.secret(ctx, args[1:])
	case "policy":
		return a.policy(ctx, args[1:])
	case "daemon":
		return a.daemon(ctx, args[1:])
	case "serve":
		return a.serve(ctx, args[1:])
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
	a.fromEnv()

	flags := flag.NewFlagSet("shard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&a.Root, "root", a.Root, "where shard keeps its state")
	flags.DurationVar(&a.Timeout, "timeout", a.Timeout, "how long a pull may take")
	flags.Var((*hostList)(&a.Insecure), "insecure-registry", "allow plaintext http to this registry host")
	flags.StringVar(&a.Host, "host", a.Host, "the shard serve front to speak to, as https://box:2376")
	flags.StringVar(&a.TokenFile, "token-file", a.TokenFile, "the file holding the bearer token that front checks")
	flags.StringVar(&a.CAFile, "ca-file", a.CAFile, "the certificate that signed the front's own")

	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("parse the flags: %w", err)
	}

	// The fallback is the flag default, so an explicit empty or relative --root still lands here.
	if !filepath.IsAbs(a.Root) {
		return nil, fmt.Errorf("--root must be an absolute path, got %q", a.Root)
	}

	if a.Host != "" {
		remote, err := remoteClient(a.Host, a.TokenFile, a.CAFile)
		if err != nil {
			return nil, err
		}
		a.remote = remote
	}

	return flags.Args(), nil
}

// fromEnv fills the three remote flags a shell exports once rather than typing on every verb.
func (a *App) fromEnv() {
	for _, pair := range []struct {
		field *string
		name  string
	}{{&a.Host, HostEnv}, {&a.TokenFile, TokenFileEnv}, {&a.CAFile, CAFileEnv}} {
		if *pair.field == "" {
			*pair.field = os.Getenv(pair.name)
		}
	}
}

// remoteClient reads the token and the certificate, so a bad one fails before any verb dials.
func remoteClient(host, tokenFile, caFile string) (*client.Client, error) {
	token, err := serve.ReadToken(tokenFile)
	if err != nil {
		return nil, err
	}

	var ca []byte
	if caFile != "" {
		ca, err = os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read the ca file %s: %w", caFile, err)
		}
	}

	return client.NewRemote(host, token, ca)
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

// client speaks to the daemon: on the socket under the root, or to the front --host names.
func (a App) client() *client.Client {
	if a.remote != nil {
		return a.remote
	}

	c := client.New(a.Root)
	if a.clientTimeout != 0 {
		c.Timeout = a.clientTimeout
	}

	return c
}

// version prints this binary's line first, so it is on the screen even when no daemon answers.
func (a App) version(ctx context.Context) error {
	if err := a.print("client " + a.Version); err != nil {
		return err
	}

	daemon, err := a.client().Version(ctx)
	if err != nil {
		return err
	}

	return a.print("daemon " + daemon.Version)
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
