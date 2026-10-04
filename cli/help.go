package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/serve"
)

// helpWidth is the widest line a help prints, so every one reads on an 80-column terminal.
const helpWidth = 80

// verbHelp is what one verb, subcommand or noun prints for --help; the top level prints its summary.
type verbHelp struct {
	// usage is each way to call it, after the word shard.
	usage   []string
	summary string
	args    []row
	flags   []flagHelp
	// notes are paragraphs wrapped to the width; one that starts with two spaces prints as written.
	notes   []string
	example string
}

// row is one line of a two-column list: an argument, a flag or a verb, and what it is.
type row struct{ left, text string }

// flagHelp is one flag as the help spells it, as --memory <size> or -i.
type flagHelp struct {
	spell string
	text  string
	// def is the default printed after the text, which for a flag that parses a zero is the one the daemon applies.
	def string
}

// wants is what a placeholder stands for, which a refusal of a value that does not parse names.
var wants = map[string]string{
	"<size>":     "a whole size with a unit, such as 512MiB or 2GiB; only 0 goes without one",
	"<duration>": "a duration such as 10s",
	"<n>":        "a whole number",
	"<format>":   "json or table",
}

// sandboxArg is the argument every verb that acts on one sandbox takes.
var sandboxArg = row{"<id|name>", "the sandbox, by its id or by its --name"}

// signingKeyDefault is the key serve and every tokens verb use without --signing-key-file.
const signingKeyDefault = "<root>/" + serve.AuthDir + "/" + serve.SigningKeyFileName

// verbGroups is the top level: every verb once, under its heading, in the order it prints.
var verbGroups = []struct {
	title string
	verbs []string
}{
	{"Sandboxes", []string{"create", "run", "exec", "list", "logs", "inspect", "stop", "start", "remove", "pause", "resume", "fork", "cp"}},
	{"Images, snapshots, secrets and egress", []string{"pull", "image", "snapshot", "secret", "policy"}},
	{"Host and access", []string{"daemon", "info", "serve", "tokens", "version"}},
}

// sandboxFlagHelps are the flags create and run share, as sandboxFlags parses them.
var sandboxFlagHelps = []flagHelp{
	{"--name <name>", "a handle every verb takes in place of the id: lower-case letters, digits, - and _", ""},
	{"--env KEY=VALUE", "set an environment variable, repeatable", ""},
	{"--secret <NAME>", "give the guest a placeholder for a stored secret as $NAME, repeatable", ""},
	{"--policy <name>", "the egress policy the host enforces; without one, the sandbox reaches the internet but nothing private", ""},
	{"--workdir <dir>", "the directory the app and every exec start in", ""},
	{"--user <user>", "the user the app and every exec run as", ""},
	{"--memory <size>", "the memory bound; 0 is unbounded on gvisor, sysbox and runc, but firecracker and vz refuse it and need 128MiB or more", ""},
	{"--cpus <n>", "the vcpu bound as a whole number; 0 is every host cpu (on vz, up to the framework's ceiling)", ""},
	{"--disk <size>", "the disk bound for the writable layer and /tmp; 0 takes the default, and Firecracker needs at least 11MiB so its journal fits", ""},
}

// formatTableHelp and formatJSONHelp are --format on a verb that writes a table, or JSON, without one.
var (
	formatTableHelp = flagHelp{"--format <format>", "the shape of the output: json or table", string(formatTable)}
	formatJSONHelp  = flagHelp{"--format <format>", "the shape of the output: json or table", string(formatJSON)}
)

// sizeNote is how create and run read a size.
const sizeNote = "A size is a whole number with KiB, MiB or GiB (binary), or KB, MB or GB (decimal), and a part of a MiB rounds up. Only 0 goes without a unit."

// helps is the one help source, keyed by the words after shard; "" is the top level.
var helps = map[string]verbHelp{
	"": {
		usage:   []string{"[global flags] <verb> [flags] [args]"},
		summary: "shard is a single-node sandbox manager (pre-alpha).",
		flags: []flagHelp{
			{"--root <dir>", "where shard keeps its state", DefaultRoot},
			{"--remote <url>", "the https URL of the proxy in front of shard serve; verbs go there instead of the socket", ""},
			{"--token-file <path>", "a token file for --remote, which beats " + client.APIKeyEnv, ""},
			{"--ca-file <pem>", "the CA certificate that signed the certificate of the proxy in front of serve", ""},
			{"--version", "print the client version; it never fails", ""},
		},
		notes: []string{
			fmt.Sprintf("Scripts and CI export %s and %s, the token field of a shard tokens mint record, and every verb goes to shard serve.", client.RemoteEnv, client.APIKeyEnv),
			fmt.Sprintf("The token comes from --token-file, then %s, then %s; an empty variable is unset. --remote and --ca-file can also come from %s and %s.", client.APIKeyEnv, client.TokenFileEnv, client.RemoteEnv, client.CAFileEnv),
			"Run shard <verb> --help for the flags and an example of one verb.",
		},
	},
	"create": {
		usage:   []string{"create [flags] <image>", "create [flags] --snapshot <id|name>"},
		summary: "create a sandbox that runs only shard-init and print its id",
		args:    []row{{"<image>", "the image; create pulls it first when it is not on disk"}},
		flags: append(slices.Clone(sandboxFlagHelps),
			flagHelp{"--snapshot <id|name>", "start from the files of a snapshot, over the image it names, in place of an image", ""},
		),
		notes: []string{
			"The flags go before the image. Pull progress goes to stderr. The id goes to stdout once the sandbox runs.",
			"create takes no command: shard-init runs alone and the sandbox stays up until shard stop. shard run starts a sandbox with an app. To give a sandbox a policy after create, use shard policy attach.",
			"--snapshot takes the place of the image, as the snapshot names its own. create never pulls for it: the image must still be on this host at the digest the snapshot recorded. Only the provider that made the snapshot starts it. On firecracker and vz a larger --disk grows the snapshot's disk, and a smaller one is refused, as a disk only grows. With no --memory or --disk, the sandbox takes the bounds its source ran under.",
			"Shard runs no health probe. To check the workload, run shard exec on your own schedule; it exits with the code of the command.",
			sizeNote,
		},
		example: "shard create --name web --memory 512MiB python:3.12",
	},
	"run": {
		usage:   []string{"run [flags] <image> <command> [args...]"},
		summary: "create a sandbox, run the command as its app and wait for the app",
		args: []row{
			{"<image>", "the image; run pulls it first when it is not on disk"},
			{"<command>", "the app and its arguments; the image's own ENTRYPOINT and CMD never run"},
		},
		flags: append(slices.Clone(sandboxFlagHelps),
			flagHelp{"--restart <policy>", "when to start the app again inside the sandbox after it exits: no, on-failure or always", ""},
			flagHelp{"--restart-retries <n>", "how many restarts before giving up (default: no limit); --restart always takes none", ""},
			flagHelp{"--restart-backoff <duration>", "how long to wait before the first restart, in whole seconds; the wait doubles each time, up to " + strconv.Itoa(models.RestartBackoffCap) + "s", seconds(sandbox.DefaultRestartBackoff)},
			flagHelp{"-d", "the same as --detach", ""},
			flagHelp{"--detach", "print the id once the app starts, and return", ""},
		),
		notes: []string{
			"The flags go before the image. The command follows the image; an optional -- may precede it.",
			"run prints what the app writes, stdout and stderr interleaved, until the restart policy ends, then exits with the app's last code, or 128 plus the signal that ended it. It exits 125 when shard itself fails.",
			"Ctrl+C stops the app and cancels its restarts, a second Ctrl+C kills it, and a third leaves with 130. Before the sandbox is up, run waits for it, then stops the app, or kills it after a second Ctrl+C, and exits 130. The sandbox stays running until shard stop.",
			"A process in the sandbox ends its own restarts with kill -USR1 1, which also terms the app, or kill -USR2 1, which kills it.",
			sizeNote,
		},
		example: "shard run --name web --restart on-failure python:3.12 python -m http.server",
	},
	"exec": {
		usage:   []string{"exec [flags] <id|name> <argv>..."},
		summary: "run a command in a running sandbox",
		args:    []row{sandboxArg, {"<argv>", "the command to run and its arguments"}},
		flags: []flagHelp{
			{"-i", "the same as --interactive", ""},
			{"--interactive", "keep stdin open for the command", ""},
			{"-t", "the same as --tty", ""},
			{"--tty", "run the command on a terminal; it needs -i, and -it gives both", ""},
			{"--env KEY=VALUE", "set an environment variable, repeatable", ""},
			{"--workdir <dir>", "the directory the command starts in", ""},
			{"--user <user>", "the user the command runs as", ""},
		},
		notes:   []string{"The flags go before the id or name. The command follows it; an optional -- may precede the command. shard exits with the exit code of the command."},
		example: "shard exec -it web /bin/sh",
	},
	"list": {
		usage:   []string{"list [--all] [--format <format>]"},
		summary: "list the sandboxes; --all adds the stopped ones",
		flags:   []flagHelp{{"--all", "list the stopped sandboxes too", ""}, formatTableHelp},
		notes:   []string{"The columns are ID, NAME, IMAGE, STATE, UPTIME, IP, RESTART and POLICY."},
		example: "shard list --all",
	},
	"logs": {
		usage:   []string{"logs [-f] [--egress] <id|name>"},
		summary: "print what the entrypoint wrote, or the egress decisions",
		args:    []row{sandboxArg},
		flags: []flagHelp{
			{"-f", "the same as --follow", ""},
			{"--follow", "keep printing until the sandbox stops", ""},
			{"--egress", "print the egress decisions instead of the entrypoint output", ""},
		},
		example: "shard logs -f web",
	},
	"inspect": {
		usage:   []string{"inspect [--format <format>] <id|name>"},
		summary: "print the record of a sandbox as JSON",
		args:    []row{sandboxArg},
		flags:   []flagHelp{formatJSONHelp},
		example: "shard inspect web",
	},
	"stop": {
		usage:   []string{"stop <id|name>"},
		summary: "stop a sandbox; its files stay for start or snapshot create",
		args:    []row{sandboxArg},
		notes: []string{
			"stop sends SIGTERM to the entrypoint and returns as soon as it exits. An entrypoint still running after " + short(models.StopGrace) + " is killed. The grace is fixed.",
			"stop is the only verb that ends a sandbox. Its memory goes, and its files stay.",
		},
		example: "shard stop web",
	},
	"start": {
		usage:   []string{"start <id|name>"},
		summary: "run a stopped sandbox again with everything it kept",
		args:    []row{sandboxArg},
		notes:   []string{"The entrypoint starts from the beginning, over the files the last run wrote."},
		example: "shard start web",
	},
	"remove": {
		usage:   []string{"remove [--force] <id|name>"},
		summary: "delete a stopped or failed sandbox and its files",
		args:    []row{sandboxArg},
		flags: []flagHelp{
			{"--force", "stop a running or paused sandbox first, and warn rather than fail on one that does not exist", ""},
		},
		notes: []string{
			"Without --force, remove refuses a running or paused sandbox. A sandbox that is still pulling its image needs no --force: remove ends the pull.",
			"--force stops the sandbox as stop does, with the same " + short(models.StopGrace) + " grace, and then deletes it.",
		},
		example: "shard remove --force web",
	},
	"pause": {
		usage:   []string{"pause <id|name>"},
		summary: "write a checkpoint of a running sandbox and free its memory",
		args:    []row{sandboxArg},
		notes:   []string{"The daemon gives up on a pause after " + short(sandbox.DefaultPauseBudget) + ". sysbox and runc refuse pause, as does vz on macOS 13 or on Intel."},
		example: "shard pause web",
	},
	"resume": {
		usage:   []string{"resume <id|name>"},
		summary: "run a paused sandbox again from its checkpoint",
		args:    []row{sandboxArg},
		example: "shard resume web",
	},
	"fork": {
		usage:   []string{"fork [--name <name>] <id|name>"},
		summary: "copy a running sandbox, memory and files, into a new running one",
		args:    []row{{"<id|name>", "the running sandbox to copy, by its id or by its --name"}},
		flags:   []flagHelp{{"--name <name>", "a handle for the new sandbox", ""}},
		notes:   []string{"The source must be running. Fork freezes it for a moment, captures its memory and files, and lets the same sandbox run on, then starts the new one from that capture. It prints the new id. Only gvisor forks for now; the other providers refuse fork."},
		example: "shard fork --name web-2 web",
	},
	"cp": {
		usage:   []string{"cp [--user <user>] <src> <id|name>:<path>", "cp <id|name>:<path> <dst>"},
		summary: "copy a file or a directory into or out of a running sandbox",
		args: []row{
			{"<src>, <dst>", "a path on the host"},
			{"<id|name>:<path>", "a path in the sandbox"},
		},
		flags:   []flagHelp{{"--user <user>", "the user a copy into the sandbox runs as, who then owns the files; empty is the entrypoint's user", ""}},
		notes:   []string{"When the destination is a directory, the copy goes inside it under its own name. A host path with a colon in it takes ./ or / first."},
		example: "shard cp ./app web:/srv/",
	},
	"pull": {
		usage:   []string{"pull <image>"},
		summary: "pull an image and unpack its rootfs",
		args:    []row{{"<image>", "the image reference, such as python:3.12"}},
		notes:   []string{"Progress goes to stderr, and the reference and the digest to stdout. The daemon's --timeout bounds each pull."},
		example: "shard pull python:3.12",
	},
	"image": {
		usage:   []string{"image <subcommand> [flags] [args]"},
		summary: "the pulled images",
	},
	"image list": {
		usage:   []string{"image list [--format <format>]"},
		summary: "list the pulled images",
		flags:   []flagHelp{formatTableHelp},
		example: "shard image list",
	},
	"image remove": {
		usage:   []string{"image remove [--force] <image>"},
		summary: "remove a pulled image",
		args:    []row{{"<image>", "the image reference, as image list prints it"}},
		flags:   []flagHelp{{"--force", "remove it even when a sandbox or a snapshot still references it", ""}},
		example: "shard image remove python:3.12",
	},
	"image prune": {
		usage:   []string{"image prune"},
		summary: "remove every pulled image that no sandbox or snapshot references",
		notes:   []string{"A stopped sandbox and a snapshot reference their image too, so prune keeps that one."},
		example: "shard image prune",
	},
	"snapshot": {
		usage:   []string{"snapshot <subcommand> [flags] [args]"},
		summary: "file copies of stopped sandboxes",
	},
	"snapshot create": {
		usage:   []string{"snapshot create [--name <name>] <id|name>"},
		summary: "copy the files of a stopped sandbox into a snapshot and print its id",
		args:    []row{{"<id|name>", "the stopped sandbox, by its id or by its --name"}},
		flags:   []flagHelp{{"--name <name>", "a handle the snapshot verbs and create --snapshot take in place of the id: lower-case letters, digits, - and _", ""}},
		notes: []string{
			"A snapshot holds the files of the sandbox and never its memory. It outlives its source: remove the sandbox and the snapshot stays.",
			"snapshot create refuses a running or paused sandbox: stop it first. The id goes to stdout once the copy is done.",
		},
		example: "shard stop web && shard snapshot create --name web-base web",
	},
	"snapshot list": {
		usage:   []string{"snapshot list [--format <format>]"},
		summary: "list the snapshots",
		flags:   []flagHelp{formatTableHelp},
		notes:   []string{"The columns are ID, NAME, SOURCE, IMAGE, SIZE and CREATED."},
		example: "shard snapshot list",
	},
	"snapshot inspect": {
		usage:   []string{"snapshot inspect [--format <format>] <id|name>"},
		summary: "print the record of a snapshot as JSON",
		args:    []row{{"<id|name>", "the snapshot, by its id or by its --name"}},
		flags:   []flagHelp{formatJSONHelp},
		example: "shard snapshot inspect web-base",
	},
	"snapshot remove": {
		usage:   []string{"snapshot remove <id|name>"},
		summary: "delete a snapshot and its files",
		args:    []row{{"<id|name>", "the snapshot, by its id or by its --name"}},
		notes:   []string{"A sandbox created from the snapshot holds its own copy of the files, so remove never refuses one."},
		example: "shard snapshot remove web-base",
	},
	"secret": {
		usage:   []string{"secret <subcommand> [flags] [args]"},
		summary: "secrets the proxy puts in requests",
	},
	"secret set": {
		usage:   []string{"secret set --to <host>... [--placeholder <string>] <NAME> [VALUE]"},
		summary: "store a secret for the --to hosts; set it again to rotate the value",
		args: []row{
			{"<NAME>", "the variable the guest sees: upper-case letters, digits and _"},
			{"[VALUE]", "the value; without it, set reads stdin or prompts"},
		},
		flags: []flagHelp{
			{"--to <host>", "a host the value may go to, repeatable; set needs at least one", ""},
			{"--placeholder <string>", "what the guest holds in place of the value: letters, digits, _, - and . only", "mock-NAME"},
		},
		notes: []string{
			"The guest sees the placeholder, and the proxy puts the value in its place in a header of an HTTPS request to a granted host.",
			"The value comes from VALUE, or from stdin when VALUE is - or stdin is a pipe, or else from a prompt with the echo off. A VALUE on the command line is visible in the process list, so set prints a caution. Put -- before a VALUE that starts with -.",
			"Use --placeholder when an SDK checks the shape of a key.",
		},
		example: `printf '%s' "$TOKEN" | shard secret set --to api.example.com API_TOKEN`,
	},
	"secret list": {
		usage:   []string{"secret list [--format <format>]"},
		summary: "list the secrets by name, destination and placeholder, without their values",
		flags:   []flagHelp{formatTableHelp},
		example: "shard secret list",
	},
	"secret remove": {
		usage:   []string{"secret remove [--force] <NAME>"},
		summary: "remove a secret",
		args:    []row{{"<NAME>", "the secret"}},
		flags:   []flagHelp{{"--force", "remove it even when a sandbox still holds it", ""}},
		example: "shard secret remove API_TOKEN",
	},
	"secret grant": {
		usage:   []string{"secret grant <id|name> <NAME>"},
		summary: "give a created or stopped sandbox the placeholder of a stored secret",
		args:    []row{sandboxArg, {"<NAME>", "the secret"}},
		example: "shard secret grant web API_TOKEN",
	},
	"secret ungrant": {
		usage:   []string{"secret ungrant <id|name> <NAME>"},
		summary: "take the placeholder of a secret back from a created or stopped sandbox",
		args:    []row{sandboxArg, {"<NAME>", "the secret"}},
		example: "shard secret ungrant web API_TOKEN",
	},
	"policy": {
		usage:   []string{"policy <subcommand> [flags] [args]"},
		summary: "egress policies for sandboxes",
	},
	"policy create": {
		usage:   []string{"policy create [--allow <rule>]... [--deny <rule>]... <name>"},
		summary: "store an egress policy; the first rule that matches wins",
		args:    []row{{"<name>", "the policy: lower-case letters, digits and -"}},
		flags: []flagHelp{
			{"--allow <rule>", "a rule to allow, repeatable", ""},
			{"--deny <rule>", "a rule to deny, repeatable", ""},
		},
		notes: []string{
			"The rules apply in order, and the first match wins. Traffic that no rule matches is dropped.",
			"A rule is <destination> [tcp|udp[:<ports>]], where ports is a comma-separated list of numbers and ranges. The destination is a host, an address, a prefix, any, or dns:",
			"  10.0.0.0/8 tcp:22   api.example.com   any udp:53   dns",
			"An allow dns rule opens udp and tcp 53 to the sandbox nameservers. Any name rule opens them as well. A deny dns rule is refused, because dns stays closed until a rule opens it.",
			"A name rule covers tcp to ports 80 and 443 only, and both ports when it names none. A name may carry a wildcard: *.example.com matches any depth, api.*.example.com one label, and * every host. A suffix:example.com rule names the host and everything under it. Name rules match in the proxy only.",
			"A sandbox with a policy or a secret sends its web traffic through the proxy the daemon runs.",
		},
		example: "shard policy create --allow dns --allow api.example.com api-only",
	},
	"policy show": {
		usage:   []string{"policy show [--format <format>] <name>"},
		summary: "print a policy as JSON, with the sandboxes that hold it",
		args:    []row{{"<name>", "the policy"}},
		flags:   []flagHelp{formatJSONHelp},
		example: "shard policy show api-only",
	},
	"policy list": {
		usage:   []string{"policy list [--format <format>]"},
		summary: "list the policies",
		flags:   []flagHelp{formatTableHelp},
		example: "shard policy list",
	},
	"policy remove": {
		usage:   []string{"policy remove <name>"},
		summary: "remove a policy that no sandbox holds",
		args:    []row{{"<name>", "the policy"}},
		example: "shard policy remove api-only",
	},
	"policy attach": {
		usage:   []string{"policy attach <id|name> <policy>"},
		summary: "give a created or stopped sandbox a stored policy in place of the one it holds",
		args:    []row{sandboxArg, {"<policy>", "the policy"}},
		example: "shard policy attach web api-only",
	},
	"policy detach": {
		usage:   []string{"policy detach <id|name>"},
		summary: "remove the policy from a sandbox and leave its secrets as they are",
		args:    []row{sandboxArg},
		example: "shard policy detach web",
	},
	"daemon": {
		usage:   []string{"daemon [flags]", "daemon status"},
		summary: "run the resident daemon; daemon status prints its state",
		flags: []flagHelp{
			{"--provider <name>", "the provider the sandboxes run on: " + orList(daemon.Providers), ""},
			{"--timeout <duration>", "how long one image pull may take", short(DefaultTimeout)},
			{"--insecure-registry <host>", "allow plain http to this registry host, repeatable", ""},
			{"--log <path>", "the file for the daemon's output, reopened on SIGHUP so newsyslog can rotate it (Mac only)", ""},
		},
		notes: []string{
			"The daemon owns the sandboxes, the background work, the API socket and the proxy. systemd starts it, or launchd on a Mac.",
			"A root that holds records, or the data image they live in, keeps the provider that made them and refuses any other --provider. Without --provider, a fresh root takes firecracker on a Linux host whose " + daemon.KVMDevice + " opens, gvisor on one without it, and vz on macOS. shard never picks sysbox or runc; they run only when named. shard info prints the pick.",
			InitPathEnv + " names the guest supervisor on Linux (default " + DefaultInitPath + ").",
		},
		example: "shard daemon --provider gvisor",
	},
	"daemon status": {
		usage:   []string{"daemon status [--format <format>]"},
		summary: "print what the running daemon reports about itself",
		flags:   []flagHelp{formatTableHelp},
		notes:   []string{"It prints the version, pid, start time, socket, provider, capabilities and proxy ports, one per line, then the background tasks. It exits 1 when a task is in backoff."},
		example: "shard daemon status",
	},
	"serve": {
		usage:   []string{"serve [flags]"},
		summary: "expose the daemon over plain HTTP with token auth, for an HTTPS proxy in front of it",
		flags: []flagHelp{
			{"--listen <addr>", "the address to listen on; any address other than loopback carries tokens in clear text", serve.DefaultListen},
			{"--signing-key-file <path>", "the key that signs and checks every token; a named file must exist", signingKeyDefault},
			{"--tokens-file <path>", "the ledger of minted tokens, in place of the one beside the signing key file", ""},
		},
		notes: []string{
			"serve speaks plain HTTP. Put a proxy or tunnel such as Caddy, Cloudflare Tunnel or Tailscale Serve in front of it for HTTPS; docs/daemon.md has the setups.",
			"It checks the token on each request and passes the bytes to the daemon socket.",
			"serve and tokens mint create the default signing key on first use, at 0600 in a 0700 directory, and both use it after that.",
			"It runs as its own unprivileged process, and its own unit starts it. shard tokens mint makes the tokens it checks.",
		},
		example: "shard serve",
	},
	"tokens": {
		usage:   []string{"tokens <subcommand> [flags] [args]"},
		summary: "the tokens shard serve checks",
	},
	"tokens mint": {
		usage:   []string{"tokens mint --name <sub> [flags]"},
		summary: "sign a token for a subject, record it in the ledger and print it",
		flags: []flagHelp{
			{"--name <sub>", "the subject the token names; mint needs one", ""},
			{"--signing-key-file <path>", "the key that signs the token; a named file must exist", signingKeyDefault},
			{"--duration <duration>", "how long the token stays valid; without it, the token never expires", ""},
			{"--scopes <list>", "a comma-separated list of scopes, such as sandbox:read,exec; without it, the token carries every scope", ""},
			{"--tokens-file <path>", "the ledger to record the token in, in place of the one beside the signing key file", ""},
			formatJSONHelp,
		},
		notes: []string{
			"mint and serve create the default signing key on first use, at 0600 in a 0700 directory, and both use it after that.",
			"mint runs locally, so the daemon never sees the signing key. It prints the record as one line of JSON.",
		},
		example: "shard tokens mint --name build-agent --duration 24h",
	},
	"tokens list": {
		usage:   []string{"tokens list [flags]"},
		summary: "list every token the ledger records, with its status",
		flags: []flagHelp{
			{"--signing-key-file <path>", "the signing key file, whose directory holds the ledger", signingKeyDefault},
			{"--tokens-file <path>", "the ledger itself, in place of the one beside the signing key file", ""},
			formatTableHelp,
		},
		notes:   []string{"list reads the ledger and never creates a key or a file. The columns are ID, NAME, ISSUED, EXPIRES, SCOPES and STATUS."},
		example: "shard tokens list",
	},
	"tokens revoke": {
		usage:   []string{"tokens revoke [flags] <id>", "tokens revoke [flags] --name <sub>"},
		summary: "mark a token revoked, so the next request that carries it fails",
		args:    []row{{"<id>", "the token, as tokens list prints it"}},
		flags: []flagHelp{
			{"--name <sub>", "revoke every token of this subject instead of one id", ""},
			{"--signing-key-file <path>", "the signing key file, whose directory holds the ledger", signingKeyDefault},
			{"--tokens-file <path>", "the ledger itself, in place of the one beside the signing key file", ""},
		},
		notes:   []string{"revoke never creates a key. The flags go before the id. It runs locally."},
		example: "shard tokens revoke 0123456789abcdef",
	},
	"info": {
		usage:   []string{"info [--format <format>]"},
		summary: "show which provider a daemon would use on this host, and why",
		flags:   []flagHelp{formatTableHelp},
		notes:   []string{"info reads the records under the root and probes the host, as a daemon started now with no --provider would, so it works without a daemon. shard daemon status prints what the running daemon uses."},
		example: "shard info",
	},
	"version": {
		usage:   []string{"version [--format <format>]"},
		summary: "print the client and daemon versions",
		flags:   []flagHelp{formatTableHelp},
		notes:   []string{"shard --version prints the client version alone, and never fails."},
		example: "shard version",
	},
}

// helpText renders the help of one key, usage first, so every help starts the same way.
func helpText(key string) string {
	h := helps[key]
	cmd, isCommand := lookup(key)

	usage := make([]string, 0, len(h.usage))
	for i, line := range h.usage {
		lead := "       shard "
		if i == 0 {
			lead = "Usage: shard "
		}
		usage = append(usage, lead+line)
	}
	sections := []string{strings.Join(usage, "\n"), wrap("", 0, h.summary)}

	if key == "" {
		sections = append(sections, topLevel()...)
	}
	if len(cmd.subs) > 0 {
		sections = append(sections, "Subcommands:\n"+columns(subRows(cmd)))
	}
	if len(h.args) > 0 {
		sections = append(sections, "Arguments:\n"+columns(h.args))
	}
	if len(h.flags) > 0 {
		heading := "Flags:\n"
		if key == "" {
			heading = "Global flags, which go before the verb:\n"
		}
		sections = append(sections, heading+columns(flagRows(h.flags)))
	}
	for _, note := range h.notes {
		if strings.HasPrefix(note, "  ") {
			sections = append(sections, note)

			continue
		}
		sections = append(sections, wrap("", 0, note))
	}
	if isCommand && cmd.run == nil {
		sections = append(sections, wrap("", 0, fmt.Sprintf("Run shard %s <subcommand> --help for the flags and an example of one.", key)))
	}
	if h.example != "" {
		sections = append(sections, "Example:\n  "+h.example)
	}

	return strings.Join(sections, "\n\n")
}

// topLevel is the verb groups, one line per verb; a noun names its subcommands before its summary.
func topLevel() []string {
	var all []row
	groups := make([][]row, 0, len(verbGroups))
	for _, group := range verbGroups {
		var rows []row
		for _, name := range group.verbs {
			text := helps[name].summary
			if cmd, _ := lookup(name); cmd.run == nil {
				text = strings.Join(names(cmd.subs), ", ") + ": " + text
			}
			rows = append(rows, row{name, text})
		}
		groups = append(groups, rows)
		all = append(all, rows...)
	}

	// One width for every group, so the summaries line up down the whole screen.
	width := widest(all)
	sections := make([]string, 0, len(verbGroups))
	for i, group := range verbGroups {
		sections = append(sections, group.title+"\n"+table(groups[i], width))
	}

	return sections
}

func subRows(cmd command) []row {
	rows := make([]row, 0, len(cmd.subs))
	for _, sub := range cmd.subs {
		rows = append(rows, row{sub.name, helps[cmd.name+" "+sub.name].summary})
	}

	return rows
}

func flagRows(flags []flagHelp) []row {
	rows := make([]row, 0, len(flags))
	for _, f := range flags {
		text := f.text
		if f.def != "" {
			text += " (default " + f.def + ")"
		}
		rows = append(rows, row{f.spell, text})
	}

	return rows
}

func columns(rows []row) string { return table(rows, widest(rows)) }

// table lays rows out in two columns, the text wrapped under its own column so no line passes the width.
func table(rows []row, width int) string {
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lead := "  " + r.left + strings.Repeat(" ", width-len(r.left)) + "  "
		lines = append(lines, wrap(lead, len(lead), r.text))
	}

	return strings.Join(lines, "\n")
}

func widest(rows []row) int {
	width := 0
	for _, r := range rows {
		width = max(width, len(r.left))
	}

	return width
}

// wrap appends text to lead a word at a time, and starts a line indented by indent wherever the next word would pass the width.
func wrap(lead string, indent int, text string) string {
	var lines []string
	line, empty := lead, true
	for word := range strings.FieldsSeq(text) {
		if !empty && len(line)+1+len(word) > helpWidth {
			lines = append(lines, line)
			line, empty = strings.Repeat(" ", indent), true
		}
		if !empty {
			line += " "
		}
		line += word
		empty = false
	}

	return strings.Join(append(lines, line), "\n")
}

// flagName is the name a spelled flag parses as: --memory <size> is memory.
func flagName(spell string) string {
	name, _, _ := strings.Cut(strings.TrimLeft(spell, "-"), " ")

	return name
}

// placeholder is what a spelled flag takes after its name, as <size>, or nothing for a bool.
func placeholder(spell string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(spell, "-"), flagName(spell)))
}

// dashed spells a flag name the way the help does: one dash for a single letter, two for a word.
func dashed(name string) string {
	if len(name) == 1 {
		return "-" + name
	}

	return "--" + name
}

// orList joins words as a sentence does: a, b or c.
func orList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}

	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// seconds spells a count of seconds the way a duration flag takes it.
func seconds(n int) string { return short(time.Duration(n) * time.Second) }

// short spells a duration as 30s, 10m or 1h, where Go's own String says 1h0m0s.
func short(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}

	return s
}
