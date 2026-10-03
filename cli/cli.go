// Package cli defines the shard commands and parses their flags.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/serve"
)

// DefaultRoot is where shard keeps everything on the box. The client owns it: its connect hint names the unit there only.
const DefaultRoot = client.DefaultRoot

// DefaultTimeout bounds one pull inside the daemon. Without it a registry that accepts and stalls pins it.
const DefaultTimeout = 30 * time.Minute

// DefaultInitPath is where make devbox-sync installs the guest supervisor on a Linux box; a Mac daemon carries its own.
const DefaultInitPath = "/usr/local/bin/shard-init"

// InitPathEnv overrides the guest supervisor the daemon uses. It is a property of the install, so it is no create flag.
const InitPathEnv = "SHARD_INIT_PATH"

// The environment behind the three flags that point a verb at a remote daemon, as docker's DOCKER_HOST does.
const (
	RemoteEnv    = "SHARD_REMOTE"
	TokenFileEnv = "SHARD_TOKEN_FILE" //nolint:gosec // G101: this names a file, and holds no token
	CAFileEnv    = "SHARD_CA_FILE"
)

const usage = `shard - a single-node sandbox manager (pre-alpha)

Usage:
  shard create [flags] <image> [-- <argv>...]
                           create a sandbox, start its entrypoint and print its id
  shard exec [flags] <id|name> -- <argv>...
                           run a command in a sandbox that is already running
  shard start <id|name>    run a stopped sandbox again with everything it kept
  shard pause <id|name>    write a snapshot of a running sandbox and free its memory (the daemon gives up after 10 min)
  shard resume <id|name>   run a paused sandbox again from its snapshot
  shard fork [--name <name>] <id|name>
                           start a new sandbox from the snapshot of another
  shard clone [--name <name>] <id|name>
                           start a new sandbox on a copy of the files that a stopped or paused sandbox kept
  shard cp [--user <user>] <src> <id|name>:<path>
  shard cp <id|name>:<path> <dst>
                           copy a file or a directory into or out of a running sandbox
                           when the destination is a directory, the copy goes inside it under its own name
  shard stop [flags] <id|name>
                           end a sandbox and keep everything it holds
  shard rm [flags] <id|name>
                           free what a stopped sandbox still holds
  shard ls [--all]         list the sandboxes that are up, with the restart policy and the egress policy of each
                           --all lists the stopped ones too
  shard inspect <id|name>  print the record of a sandbox as JSON
  shard logs [-f] [--egress] <id|name>
                           print what the entrypoint wrote, or the egress decisions with --egress
                           -f keeps following either one
  shard pull <image>       pull an image and unpack its rootfs
  shard image ls           list the pulled images
  shard image rm [--force] <image>
                           remove a pulled image (with --force, even one a sandbox still references)
  shard image prune        remove every pulled image that no sandbox references
  shard secret set --to <host>... [--placeholder <string>] <NAME> [VALUE]
                           store a secret granted to those hosts, and set it again to rotate the value
                           the guest sees the placeholder, and the proxy replaces it with the value in a header of an HTTPS request to a granted host
                           the value comes from VALUE, or from stdin when VALUE is - or stdin is a pipe, or else from a prompt with echo off
                           put -- before a VALUE that starts with -
                           use --placeholder to replace the default mock-NAME when an SDK checks the shape of a key
  shard secret ls          list the secrets by name, destination and placeholder, without their values
  shard secret rm [--force] <NAME>
                           remove a secret (with --force, even one a sandbox still holds)
  shard secret grant <id|name> <NAME>
                           give a created or stopped sandbox the placeholder of a stored secret
  shard secret ungrant <id|name> <NAME>
                           take that placeholder back from the sandbox
  shard policy create [--allow <rule>]... [--deny <rule>]... <name>
                           store an egress policy whose rules apply in order, where the first match wins
                           traffic that no rule matches is dropped
  shard policy show <name> print a policy as JSON, with the sandboxes that hold it
  shard policy ls          list the policies
  shard policy rm <name>   remove a policy that no sandbox holds
  shard policy attach <id|name> <policy>
                           give a created or stopped sandbox a stored policy in place of the one it holds
  shard policy detach <id|name>
                           remove the policy from a sandbox and leave its secrets as they are
  shard daemon [--log <path>]
                           run the resident process that owns the sandbox lifecycle, the background work, the API socket and the proxy
                           systemd starts it, or launchd on a Mac
  shard daemon status      print the version, pid, start time, socket, provider, capabilities and proxy ports of the daemon, one per line
  shard serve [flags]      accept TLS on a TCP address, check the token on each request and pass the bytes to the daemon socket
                           its own unit starts it
  shard tokens mint --name <sub> [--duration <dur>] [--scopes <list>] [--tokens-file <path>] --secret-file <path>
                           sign one token for a subject, record it in the ledger and print it
                           without --duration the token never expires, and without --scopes it carries every scope
                           the verb runs locally, so the daemon never sees the secret
  shard tokens ls --secret-file <path>
                           list every token the ledger records, with its id, subject, issued and expiry times, scopes and status
  shard tokens revoke [--name <sub>] --secret-file <path> <id>
                           mark a token revoked so the next request that carries it fails
                           --name revokes every token of a subject
                           the verb runs locally
  shard info               print the substrate that a daemon started now on this root would run on, and why
                           it asks the host rather than the socket, so it answers before the socket exists
  shard version            print the version of this binary and of the daemon
                           --version prints only the version of this binary and never fails

A rule is <destination> [tcp|udp[:<ports>]], where ports is a comma-separated list of numbers and ranges.
The destination is a host, an address, a prefix, any, or dns:
  10.0.0.0/8 tcp:22   api.example.com   any udp:53   dns
An allow dns rule opens udp and tcp 53 to the sandbox nameservers. Any name rule opens them as well.
A deny dns rule is refused, because dns stays closed until a rule opens it.
A name rule covers tcp to ports 80 and 443 only, and both ports when it names none. A name may carry
a wildcard: *.example.com matches any depth, api.*.example.com one label, and * every host.
A suffix:example.com rule names the host and everything under it. Name rules match in the proxy only.
A sandbox with a policy or a secret is fronted, which means its web traffic goes through the proxy the daemon runs.

Create flags, which must precede the image:
  --name <name>            a handle every verb takes in place of the id
  --env KEY=VALUE          set an environment variable, repeatable
  --secret <NAME>          give the guest a placeholder for a stored secret as $NAME, repeatable
  --policy <name>          the egress policy the host enforces (without one, the sandbox reaches the internet but nothing private)
                           to give an existing sandbox a policy, use shard policy attach
  --workdir <dir>          the directory the entrypoint starts in
  --user <user>            the user the entrypoint runs as
  --memory <MiB>           the memory bound; 0 is unbounded on Linux, but vz refuses it because the VM needs a size
  --cpus <n>               the vcpu bound as a whole number; 0 is every host cpu (on vz, up to the framework's ceiling)
  --restart-on-oom[=N]     start the sandbox again when the host ends it for its memory; bare is unlimited, =N caps the starts, and it needs --memory
  --restart <policy>       when to start the entrypoint again inside the sandbox after it exits: no, on-failure or always
  --restart-retries <n>    how many times the supervisor starts the entrypoint again before it gives up, unlimited by default; the always policy refuses a count
  --restart-backoff <dur>  how long to wait before the first start again, in whole seconds, 1s by default; the wait doubles each time, up to 60s
  --health-command <cmd>   a shell command the daemon runs in the sandbox every interval; exit 0 is a pass
  --health-interval <dur>  the time between two probes, in whole seconds (30s by default, 1h at most)
  --health-timeout <dur>   how long one probe has to answer, in whole seconds (10s by default, 10m at most)
  --health-retries <n>     how many failed probes in a row mark the sandbox unhealthy (3 by default)

Exec flags, which must precede the id or name:
  -i                       keep stdin open for the command
  -t                       run the command on a terminal; use -it for both
  --env KEY=VALUE          set an environment variable, repeatable
  --workdir <dir>          the directory the command starts in
  --user <user>            the user the command runs as

Stop flags, which must precede the id or name:
  --time <duration>        how long the entrypoint gets before it is killed (default 10s)

Rm flags, which must precede the id or name:
  --force                  stop the sandbox first if it is still up
  --time <duration>        how long --force gives the entrypoint before it is killed

Daemon flags:
  --log <path>             the file for the daemon's output, reopened on SIGHUP so newsyslog can rotate it (Mac only)

Serve flags:
  --listen <addr>          the address to listen on (default ` + serve.DefaultListen + `)
  --cert <pem>             the tls certificate to serve; serve refuses to start without a certificate and key pair
  --key <pem>              the key for that certificate
  --secret-file <path>     the file holding the secret that signs and checks every token

Tokens mint flags:
  --name <sub>             the subject the token names
  --duration <dur>         how long the token stays valid; the default, 0, never expires
  --secret-file <path>     the file holding the secret that signs the token
  --tokens-file <path>     the ledger to record the token in, instead of the one beside the secret file
  --scopes <list>          a comma-separated list of scopes the token carries; empty means every verb

Flags:
  --root <dir>             where shard keeps its state (default ` + DefaultRoot + `)
  --timeout <duration>     how long a pull may take, read by the daemon (default 30m)
  --insecure-registry <host>
                           allow plaintext http to this registry host, repeatable
  --provider <name>        the substrate the daemon runs sandboxes on: gvisor, sysbox, runc, vz or firecracker
                           (a root that holds records, or the data image they live in, keeps the provider that made
                           them and refuses any other name. Without this flag, a root that holds neither picks
                           firecracker on a Linux host whose ` + daemon.KVMDevice + ` opens, gvisor on one without it, and vz on macOS.
                           shard never picks sysbox or runc for a host; they run only when named here)
  --remote <url>           talk to a shard serve front, such as https://box:2376, instead of the socket
  --token-file <path>      the file holding the bearer token that front checks
  --ca-file <pem>          the certificate that signed the front's certificate

--remote, --token-file and --ca-file can also come from ` + RemoteEnv + `, ` + TokenFileEnv + ` and ` + CAFileEnv + `.

Run shard <verb> --help to print the flags of one verb, and shard --version for the client version.`

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
	// InitPath is the host path of the guest supervisor. It defaults to the environment when empty, and stays empty on a Mac.
	InitPath string
	// Provider names the substrate the daemon runs sandboxes on. Empty lets the root and the host pick, as shard info prints.
	Provider string
	// Remote is the shard serve front a verb speaks to instead of the socket, as https://box:2376.
	Remote string
	// TokenFile holds the bearer token that front checks, and CAFile the certificate that signed its own.
	TokenFile string
	CAFile    string

	// remote is the client of Remote, built once the globals are parsed and before any verb runs.
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
	err := a.run(ctx, args)
	// A --help or --version anywhere surfaces as a printExit: print its text and exit 0.
	var exit printExit
	if errors.As(err, &exit) {
		return a.print(exit.text)
	}

	return err
}

// printExit carries the text a verb means to print before it exits 0, as --help and --version do.
type printExit struct{ text string }

func (p printExit) Error() string { return p.text }

// parseVerb parses a verb's flags and turns a --help request into a printExit Run prints, exit 0.
func parseVerb(flags *flag.FlagSet, args []string) error {
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return printExit{text: usageOf(flags)}
	}

	return err
}

// usageOf renders a verb's own flags the way the flag package would, so no help text is hand-written.
func usageOf(flags *flag.FlagSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage of %s:\n", flags.Name())
	flags.SetOutput(&b)
	flags.PrintDefaults()
	flags.SetOutput(io.Discard)

	return b.String()
}

func (a App) run(ctx context.Context, args []string) error {
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
	case "cp":
		return a.cp(ctx, args[1:])
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
	case "tokens":
		return a.tokens(args[1:])
	case "info":
		return a.info(args[1:])
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
	var showVersion bool
	flags.BoolVar(&showVersion, "version", false, "print the client version and exit")
	flags.StringVar(&a.Root, "root", a.Root, "where shard keeps its state")
	flags.DurationVar(&a.Timeout, "timeout", a.Timeout, "how long a pull may take")
	flags.Var((*hostList)(&a.Insecure), "insecure-registry", "allow plaintext http to this registry host")
	flags.StringVar(&a.Provider, "provider", a.Provider, "the substrate the daemon runs sandboxes on")
	flags.StringVar(&a.Remote, "remote", a.Remote, "the shard serve front to talk to, such as https://box:2376")
	flags.StringVar(&a.TokenFile, "token-file", a.TokenFile, "the file holding the bearer token that front checks")
	flags.StringVar(&a.CAFile, "ca-file", a.CAFile, "the certificate that signed the front's certificate")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, printExit{text: usage}
		}

		return nil, fmt.Errorf("parse the flags: %w", err)
	}

	// --version answers before the root is checked, so it never fails.
	if showVersion {
		return nil, printExit{text: "client " + a.Version}
	}

	// The fallback is the flag default, so an explicit empty or relative --root still lands here.
	if !filepath.IsAbs(a.Root) {
		return nil, fmt.Errorf("--root must be an absolute path, got %q", a.Root)
	}

	if a.Remote != "" {
		remote, err := remoteClient(a.Remote, a.TokenFile, a.CAFile)
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
	}{{&a.Remote, RemoteEnv}, {&a.TokenFile, TokenFileEnv}, {&a.CAFile, CAFileEnv}} {
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

// initPathFromEnv resolves where the guest supervisor lives on this host; empty on a Mac, whose daemon installs the one it embeds.
func initPathFromEnv() string {
	if path := os.Getenv(InitPathEnv); path != "" {
		return path
	}
	if runtime.GOOS == "darwin" {
		return ""
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

// client speaks to the daemon: on the socket under the root, or to the front --remote names.
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

	d, err := a.client().Version(ctx)
	if err != nil {
		return err
	}
	if err := a.print("daemon " + d.Version); err != nil {
		return err
	}
	if line := shimLine(); line != "" {
		return a.print(line)
	}

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
