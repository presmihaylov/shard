package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// about is the sentence its own help opens with, when it says more than the summary does.
	about string
	args  []row
	flags []flagHelp
	// env is the variables it reads, which only the top level lists.
	env   []row
	notes []note
	// examples print as written, one call per line, so each pastes whole however wide it is.
	examples []string
}

// row is one line of a two-column list: an argument, a flag or a verb, and what it is.
type row struct{ left, text string }

// note is a part of a help after its options: a paragraph, or a titled table and the lines under it.
type note struct {
	title string
	rows  []row
	// lines keep their breaks and the indent each starts with; a long one wraps under itself.
	lines []string
}

// para is a note with no title, one line per sentence the help prints.
func para(lines ...string) note { return note{lines: lines} }

// flagHelp is one flag as the help spells it, as --memory <size> or -f, --follow.
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

// The arguments more than one verb takes.
var (
	imageArg    = row{"IMAGE", "image to use; downloaded if needed"}
	sandboxArg  = row{"SANDBOX", "sandbox ID or name"}
	sourceArg   = row{"SANDBOX", "source sandbox ID or name"}
	argsArg     = row{"ARGS", "arguments for the command"}
	snapshotArg = row{"SNAPSHOT", "snapshot ID or name"}
	secretArg   = row{"NAME", "secret name"}
	policyArg   = row{"NAME", "policy name"}
)

// signingKeyDefault is the key serve and every tokens verb use without --signing-key-file.
const signingKeyDefault = "<root>/" + serve.AuthDir + "/" + serve.SigningKeyFileName

// verbGroups is the top level: every verb once, under its heading, in the order it prints.
var verbGroups = []struct {
	title string
	verbs []string
}{
	{"Sandboxes", []string{"create", "run", "exec", "list", "logs", "inspect", "stop", "start", "remove", "pause", "resume", "fork", "cp"}},
	{"Images, snapshots, secrets and network policies", []string{"pull", "image", "snapshot", "secret", "policy"}},
	{"Host and access", []string{"capabilities", "daemon", "info", "serve", "tokens", "version"}},
}

// sandboxFlagHelps are the flags create and run share, as sandboxFlags parses them.
var sandboxFlagHelps = []flagHelp{
	{"--name <name>", "sandbox name to use instead of its ID", ""},
	{"--env KEY=VALUE", "set an environment variable; repeatable", ""},
	{"--secret <NAME>", "let the sandbox use a stored secret; repeatable", ""},
	{"--policy <name>", "outbound network policy", ""},
	{"--workdir <dir>", "default directory for commands", ""},
	{"--user <user>", "default user for commands", ""},
	{"--memory <size>", "memory limit", ""},
	{"--vcpus <n>", "CPU count; 0 uses all available host CPUs", ""},
	{"--disk <size>", "disk limit; 0 uses the default", ""},
}

// The flags more than one verb takes.
var (
	formatTableHelp = flagHelp{"--format <format>", "output format: json or table", string(formatTable)}
	formatJSONHelp  = flagHelp{"--format <format>", "output format: json or table", string(formatJSON)}
	signingKeyHelp  = flagHelp{"--signing-key-file <path>", "token signing key", signingKeyDefault}
	// The read-only tokens verbs find the registry through the key and never create one.
	registryKeyHelp = flagHelp{"--signing-key-file <path>", "locate the token registry beside this key", signingKeyDefault}
)

// The notes more than one verb prints.
var (
	namesNote   = note{title: "Names", lines: []string{"Use lower-case letters, digits, hyphens and underscores."}}
	secretsNote = note{title: "Secrets", lines: []string{
		"Store a secret with 'shard secret set', then select it with --secret.",
		"Commands receive $NAME with a placeholder instead of the secret value.",
		"Shard replaces the placeholder with the secret in HTTPS request headers",
		"sent to approved destinations. The secret value stays outside the sandbox.",
	}}
	networkNote = note{title: "Network access", lines: []string{
		noPolicyLine,
		"Use 'shard policy attach' to assign a policy after creation.",
	}}
	limitsNote     = note{title: "Resource limits", lines: []string{"Use sizes such as 512MiB or 2GiB, and whole numbers for CPUs."}}
	signingKeyNote = para("The default signing key is created automatically on first use.", "A custom signing key file must already exist.")
	hostOnlyNote   = para("Runs only on the daemon host and refuses --remote and " + client.RemoteEnv + ".")
)

const noPolicyLine = "Without a policy, the sandbox can access the internet but not private networks."

// helps is the one help source, keyed by the words after shard; "" is the top level.
var helps = map[string]verbHelp{
	"": {
		usage: []string{"[OPTIONS] COMMAND [ARGS...]"},
		about: "A runtime for isolated sandboxes on your own infrastructure. (pre-alpha)",
		flags: []flagHelp{
			{"--root <dir>", "directory for local Shard data", DefaultRoot},
			{"--remote <url>", "URL of the Shard API server; HTTP/HTTPS supported,\nHTTPS recommended", ""},
			{"--version", "show the client version", ""},
		},
		env: []row{
			{client.RemoteEnv, "API server URL; --remote overrides it"},
			{client.APIKeyEnv, "API token from shard tokens mint"},
			{client.CAFileEnv, "custom CA certificate file; HTTPS only"},
		},
		notes: []note{
			para(fmt.Sprintf("Set %s and %s for remote access.", client.RemoteEnv, client.APIKeyEnv), "Without a remote URL, Shard connects to the local daemon."),
		},
	},
	"create": {
		usage:   []string{"create [OPTIONS] IMAGE", "create [OPTIONS] --snapshot SNAPSHOT"},
		summary: "create a sandbox and print its ID",
		args:    []row{imageArg},
		flags: append(slices.Clone(sandboxFlagHelps),
			flagHelp{"--snapshot <id|name>", "create from a filesystem snapshot instead of an image", ""},
		),
		notes: []note{
			para(
				"The sandbox starts without a main command.",
				"Use 'shard exec' to execute commands or 'shard run' to create one with a main command.",
				"The sandbox stays active until stopped.",
			),
			namesNote, secretsNote, networkNote, limitsNote,
		},
		examples: []string{
			"shard create --name web --memory 512MiB python:3.12",
			"shard create --name web-copy --snapshot web-files",
			"shard create --name worker --secret API_TOKEN python:3.12",
		},
	},
	"run": {
		usage:   []string{"run [OPTIONS] IMAGE COMMAND [ARGS...]"},
		summary: "create a sandbox and run a command in the foreground",
		args:    []row{imageArg, {"COMMAND", "main command to execute"}, argsArg},
		flags: append(slices.Clone(sandboxFlagHelps),
			flagHelp{"--restart <policy>", "restart policy: no, on-failure or always", ""},
			flagHelp{"--restart-retries <n>", "maximum restarts; default unlimited", ""},
			flagHelp{"--restart-backoff <duration>", "initial restart delay, up to " + seconds(models.RestartBackoffCap), seconds(sandbox.DefaultRestartBackoff)},
			flagHelp{"-d, --detach", "run in the background and print the sandbox ID", ""},
		),
		notes: []note{
			para(
				"The specified command replaces the image's default command.",
				"Command output appears in your terminal unless you use --detach.",
				"The sandbox stays active after the command exits.",
			),
			para("Press Ctrl+C to stop the command and cancel its restarts.", "Use 'shard stop' to stop the sandbox."),
			namesNote, secretsNote, networkNote,
			{
				title: "Restarts",
				rows: []row{
					{string(models.RestartNo), "do not restart the command (default)"},
					{string(models.RestartOnFailure), "restart after a nonzero exit"},
					{string(models.RestartAlways), "restart after any exit"},
				},
				lines: []string{
					"--restart-retries applies to on-failure only.",
					"Restart delays double after each restart, up to " + strconv.Itoa(models.RestartBackoffCap) + "s.",
				},
			},
			limitsNote,
		},
		examples: []string{
			"shard run --name web python:3.12 python -m http.server",
			"shard run --detach --name web --restart on-failure python:3.12 python -m http.server",
		},
	},
	"exec": {
		usage:   []string{"exec [OPTIONS] SANDBOX COMMAND [ARGS...]"},
		summary: "execute a command in a running sandbox",
		args:    []row{sandboxArg, {"COMMAND", "command to execute"}, argsArg},
		flags: []flagHelp{
			{"-i, --interactive", "keep standard input open", ""},
			{"-t, --tty", "use a terminal; requires --interactive", ""},
			{"--env KEY=VALUE", "set an environment variable for this command; repeatable", ""},
			{"--workdir <dir>", "directory for this command", ""},
			{"--user <user>", "user for this command", ""},
		},
		notes:    []note{para("The command uses the sandbox's default directory and user unless overridden.", "Shard returns the command's exit code.")},
		examples: []string{"shard exec web python script.py", "shard exec --workdir /app web npm test", "shard exec -it web /bin/sh"},
	},
	"list": {
		usage:   []string{"list [OPTIONS]"},
		summary: "list active sandboxes",
		flags: []flagHelp{
			{"--all", "include stopped sandboxes", ""},
			formatTableHelp,
		},
		notes:    []note{para("Table columns: ID, NAME, IMAGE, STATE, UPTIME, RESTART and POLICY.")},
		examples: []string{"shard list", "shard list --all", "shard list --format json"},
	},
	"logs": {
		usage:    []string{"logs [OPTIONS] SANDBOX"},
		summary:  "show output from the sandbox's main command",
		args:     []row{sandboxArg},
		flags:    []flagHelp{{"-f, --follow", "show new output until the sandbox stops", ""}},
		examples: []string{"shard logs web", "shard logs --follow web"},
	},
	"inspect": {
		usage:    []string{"inspect [OPTIONS] SANDBOX"},
		summary:  "show detailed information about a sandbox",
		args:     []row{sandboxArg},
		flags:    []flagHelp{formatJSONHelp},
		examples: []string{"shard inspect web", "shard inspect --format table web"},
	},
	"stop": {
		usage:   []string{"stop SANDBOX"},
		summary: "stop a sandbox and preserve its files",
		args:    []row{sandboxArg},
		notes: []note{
			para(
				"The main command has up to "+strconv.Itoa(int(models.StopGrace/time.Second))+" seconds to exit before it is terminated.",
				"Files remain available, but memory and process state are lost.",
			),
			para("Use 'shard start' to start the sandbox again.", "Use 'shard snapshot create' to save its files as a snapshot."),
		},
		examples: []string{"shard stop web"},
	},
	"start": {
		usage:    []string{"start SANDBOX"},
		summary:  "start a stopped sandbox with its saved files",
		args:     []row{sandboxArg},
		notes:    []note{para("If the sandbox has a main command, it starts from the beginning.")},
		examples: []string{"shard start web"},
	},
	"remove": {
		usage:   []string{"remove [OPTIONS] SANDBOX"},
		summary: "delete a sandbox and its files",
		args:    []row{sandboxArg},
		flags:   []flagHelp{{"--force", "stop the sandbox first if needed; ignore a missing sandbox", ""}},
		notes: []note{para(
			"Stop a running or paused sandbox before removal, or use --force.",
			"A sandbox with an image download in progress can be removed directly.",
		)},
		examples: []string{"shard remove web", "shard remove --force web"},
	},
	"pause": {
		usage:   []string{"pause SANDBOX"},
		summary: "save a sandbox's state and suspend it",
		args:    []row{sandboxArg},
		notes: []note{para(
			"Memory and files are saved so its processes can continue after resume.",
			"The sandbox must be running. Availability depends on the provider.",
		)},
		examples: []string{"shard pause web"},
	},
	"resume": {
		usage:    []string{"resume SANDBOX"},
		summary:  "resume a paused sandbox from its saved state",
		args:     []row{sandboxArg},
		notes:    []note{para("Processes continue from where they paused.")},
		examples: []string{"shard resume web"},
	},
	"fork": {
		usage:   []string{"fork [OPTIONS] SANDBOX"},
		summary: "create a sandbox from a running sandbox's memory and files",
		args:    []row{sourceArg},
		flags:   []flagHelp{{"--name <name>", "name for the new sandbox", ""}},
		notes: []note{para(
			"The source briefly pauses, then continues running.",
			"The new sandbox starts from the captured state and has independent files.",
			"Prints the new sandbox ID. Availability depends on the provider.",
		)},
		examples: []string{"shard fork --name web-copy web"},
	},
	"cp": {
		usage:   []string{"cp [OPTIONS] SOURCE SANDBOX:PATH", "cp SANDBOX:PATH DESTINATION"},
		summary: "copy files or directories between your machine and a running sandbox",
		args: []row{
			{"SOURCE, DESTINATION", "path on your machine"},
			{"SANDBOX:PATH", "sandbox ID or name, followed by a path"},
		},
		flags: []flagHelp{{"--user <user>", "user for copies into the sandbox; defaults to its user", ""}},
		notes: []note{para(
			"When the destination is a directory, the source is copied inside it.",
			"Prefix local paths that contain a colon with './' or '/'.",
			"Successful copies produce no output.",
		)},
		examples: []string{"shard cp ./app web:/srv/", "shard cp web:/tmp/results.json ./results.json"},
	},
	"pull": {
		usage:    []string{"pull IMAGE"},
		summary:  "download an image",
		args:     []row{{"IMAGE", "image reference, such as python:3.12"}},
		notes:    []note{para("Prints the image reference and digest when the download finishes."), hostOnlyNote},
		examples: []string{"shard pull python:3.12"},
	},
	"image": {
		usage:   []string{"image COMMAND [OPTIONS] [ARGS...]"},
		summary: "manage downloaded images",
	},
	"image list": {
		usage:    []string{"image list [OPTIONS]"},
		summary:  "list downloaded images",
		flags:    []flagHelp{formatTableHelp},
		notes:    []note{hostOnlyNote},
		examples: []string{"shard image list", "shard image list --format json"},
	},
	"image remove": {
		usage:    []string{"image remove [OPTIONS] IMAGE"},
		summary:  "delete a downloaded image",
		args:     []row{{"IMAGE", "image reference"}},
		flags:    []flagHelp{{"--force", "delete the image even if a sandbox or snapshot uses it", ""}},
		notes:    []note{hostOnlyNote},
		examples: []string{"shard image remove python:3.12"},
	},
	"image prune": {
		usage:    []string{"image prune"},
		summary:  "delete unused images",
		about:    "Delete images that no sandbox or snapshot uses.",
		notes:    []note{para("Images used by stopped sandboxes are also kept."), hostOnlyNote},
		examples: []string{"shard image prune"},
	},
	"snapshot": {
		usage:   []string{"snapshot COMMAND [OPTIONS] [ARGS...]"},
		summary: "save and manage filesystem snapshots",
	},
	"snapshot create": {
		usage:   []string{"snapshot create [OPTIONS] SANDBOX"},
		summary: "save a stopped sandbox's files as a snapshot",
		about:   "Save a stopped sandbox's files as a snapshot and print its ID.",
		args:    []row{sourceArg},
		flags:   []flagHelp{{"--name <name>", "snapshot name to use instead of its ID", ""}},
		notes: []note{para(
			"Stop the sandbox first.",
			"Snapshots contain files, not memory or process state.",
			"They remain available after the source sandbox is removed.",
		)},
		examples: []string{"shard stop web", "shard snapshot create --name web-base web"},
	},
	"snapshot list": {
		usage:    []string{"snapshot list [OPTIONS]"},
		summary:  "list snapshots",
		about:    "List filesystem snapshots.",
		flags:    []flagHelp{formatTableHelp},
		notes:    []note{para("Table columns: ID, NAME, SOURCE, IMAGE, SIZE and CREATED.")},
		examples: []string{"shard snapshot list", "shard snapshot list --format json"},
	},
	"snapshot inspect": {
		usage:    []string{"snapshot inspect [OPTIONS] SNAPSHOT"},
		summary:  "show detailed information about a snapshot",
		args:     []row{snapshotArg},
		flags:    []flagHelp{formatJSONHelp},
		examples: []string{"shard snapshot inspect web-base", "shard snapshot inspect --format table web-base"},
	},
	"snapshot remove": {
		usage:    []string{"snapshot remove SNAPSHOT"},
		summary:  "delete a snapshot and its files",
		args:     []row{snapshotArg},
		notes:    []note{para("Sandboxes created from the snapshot keep their own files.")},
		examples: []string{"shard snapshot remove web-base"},
	},
	"secret": {
		usage:   []string{"secret COMMAND [OPTIONS] [ARGS...]"},
		summary: "manage secrets and sandbox access to them",
		notes: []note{para(
			"Secret values stay outside the sandbox.",
			"Shard inserts them into HTTPS request headers sent to approved destinations.",
		)},
	},
	"secret set": {
		usage:   []string{"secret set [OPTIONS] NAME [VALUE]"},
		summary: "store or update a secret",
		args: []row{
			{"NAME", "secret name; upper-case letters, digits and underscores"},
			{"VALUE", "secret value; omit it to read from input or a hidden prompt"},
		},
		flags: []flagHelp{
			{"--dest, --destination <host>", "approved destination; required and repeatable", ""},
			{"--placeholder <string>", "value visible inside the sandbox", "mock-NAME"},
		},
		notes: []note{
			para(
				"The sandbox receives the placeholder instead of the secret value.",
				"Shard replaces it in HTTPS request headers sent to approved destinations.",
			),
			para(
				"Set an existing secret again to update its value.",
				"Custom placeholders can contain letters, digits, underscores, hyphens and dots.",
			),
			para("Avoid secret values in command arguments. Use input or the hidden prompt."),
		},
		examples: []string{
			"shard secret set --destination api.example.com API_TOKEN",
			`printf '%s' "$TOKEN" | shard secret set --destination api.example.com API_TOKEN`,
		},
	},
	"secret list": {
		usage:    []string{"secret list [OPTIONS]"},
		summary:  "list secrets without their values",
		flags:    []flagHelp{formatTableHelp},
		notes:    []note{para("Shows secret names, approved destinations, placeholders and update times.")},
		examples: []string{"shard secret list", "shard secret list --format json"},
	},
	"secret remove": {
		usage:    []string{"secret remove [OPTIONS] NAME"},
		summary:  "delete a secret",
		about:    "Delete a stored secret.",
		args:     []row{secretArg},
		flags:    []flagHelp{{"--force", "delete the secret even if a sandbox uses it", ""}},
		examples: []string{"shard secret remove API_TOKEN"},
	},
	"secret grant": {
		usage:   []string{"secret grant SANDBOX NAME"},
		summary: "let a sandbox use a secret",
		about:   "Let a sandbox use a stored secret.",
		args:    []row{sandboxArg, secretArg},
		notes: []note{para(
			"The sandbox must be created or stopped.",
			"Its commands receive the secret's placeholder as $NAME.",
		)},
		examples: []string{"shard secret grant web API_TOKEN"},
	},
	"secret ungrant": {
		usage:    []string{"secret ungrant SANDBOX NAME"},
		summary:  "remove a sandbox's access to a secret",
		args:     []row{sandboxArg, secretArg},
		notes:    []note{para("The sandbox must be created or stopped.")},
		examples: []string{"shard secret ungrant web API_TOKEN"},
	},
	"policy": {
		usage:   []string{"policy COMMAND [OPTIONS] [ARGS...]"},
		summary: "manage outbound network rules and view network logs",
	},
	"policy create": {
		usage:   []string{"policy create [OPTIONS] NAME"},
		summary: "store a network policy",
		args:    []row{{"NAME", "policy name; up to 64 lower-case letters, digits and hyphens"}},
		flags: []flagHelp{
			{"--allow <rule>", "allow matching traffic; repeatable", ""},
			{"--deny <rule>", "deny matching traffic; repeatable", ""},
		},
		notes: []note{
			para("The name starts with a letter or a digit."),
			para("Rules apply in the order given. The first match decides access.", "Traffic that no rule allows is denied."),
			{title: "Rules", lines: []string{
				"Use a destination with an optional protocol and ports:",
				"  DESTINATION [tcp|udp[:PORTS]]",
				"",
				"Destinations can be domains, IP addresses, network ranges, 'any' or 'dns'.",
				"'*.example.com' matches the names under example.com, not example.com itself.",
				"'suffix:example.com' matches example.com and every name under it.",
				"Ports can be numbers or ranges, separated by commas.",
				"Domain rules default to TCP ports 80 and 443.",
				"Domain rules also allow the DNS access needed to resolve their names.",
				"Use '--allow dns' to allow DNS explicitly. '--deny dns' is not supported.",
			}},
		},
		examples: []string{
			"shard policy create --allow api.example.com api-only",
			`shard policy create --allow "10.0.0.0/8 tcp:22" internal-ssh`,
		},
	},
	"policy show": {
		usage:    []string{"policy show [OPTIONS] NAME"},
		summary:  "show a policy and the sandboxes that use it",
		args:     []row{policyArg},
		flags:    []flagHelp{formatJSONHelp},
		examples: []string{"shard policy show api-only", "shard policy show --format table api-only"},
	},
	"policy list": {
		usage:    []string{"policy list [OPTIONS]"},
		summary:  "list network policies",
		flags:    []flagHelp{formatTableHelp},
		examples: []string{"shard policy list", "shard policy list --format json"},
	},
	"policy remove": {
		usage:    []string{"policy remove NAME"},
		summary:  "delete an unused policy",
		about:    "Delete an unused network policy.",
		args:     []row{policyArg},
		notes:    []note{para("Detach the policy from its sandboxes before removal.")},
		examples: []string{"shard policy remove api-only"},
	},
	"policy attach": {
		usage:   []string{"policy attach SANDBOX POLICY"},
		summary: "assign a policy to a sandbox",
		about:   "Assign a network policy to a sandbox.",
		args:    []row{sandboxArg, {"POLICY", "policy name"}},
		notes: []note{para(
			"The sandbox must be created or stopped.",
			"The policy replaces any policy already assigned to the sandbox.",
		)},
		examples: []string{"shard policy attach web api-only"},
	},
	"policy detach": {
		usage:    []string{"policy detach SANDBOX"},
		summary:  "remove a sandbox's policy",
		about:    "Remove a sandbox's network policy.",
		args:     []row{sandboxArg},
		notes:    []note{para("The sandbox must be created or stopped.", "Secret access remains unchanged.", noPolicyLine)},
		examples: []string{"shard policy detach web"},
	},
	"policy logs": {
		usage:    []string{"policy logs [OPTIONS] SANDBOX"},
		summary:  "show network policy decisions for a sandbox",
		args:     []row{sandboxArg},
		flags:    []flagHelp{{"-f, --follow", "show new decisions as they occur", ""}},
		notes:    []note{para("Prints JSON records with destinations, decisions and the rules responsible.")},
		examples: []string{"shard policy logs web", "shard policy logs --follow web"},
	},
	"daemon": {
		usage:   []string{"daemon [OPTIONS]", "daemon status [OPTIONS]"},
		summary: "start the daemon or show its status",
		flags: []flagHelp{
			{"--provider <name>", "sandbox provider: " + orList(daemon.Providers), ""},
			{"--timeout <duration>", "image download timeout", short(DefaultTimeout)},
			{"--insecure-registry <host>", "allow HTTP for a registry; repeatable", ""},
			{"--log <path>", "daemon log file (macOS only)", ""},
		},
		notes: []note{
			para(
				"The daemon manages local sandboxes and stays active until stopped.",
				"An existing data directory must use its original provider.",
				"Use 'shard info' to see the default provider for this host.",
			),
			hostOnlyNote,
		},
		examples: []string{"shard daemon", "shard daemon --provider gvisor"},
	},
	"daemon status": {
		usage:    []string{"daemon status [OPTIONS]"},
		summary:  "show daemon status",
		flags:    []flagHelp{formatTableHelp},
		notes:    []note{para("Shows the version, provider, process details and background tasks."), hostOnlyNote},
		examples: []string{"shard daemon status", "shard daemon status --format json"},
	},
	"capabilities": {
		usage:   []string{"capabilities [OPTIONS]"},
		summary: "show the lifecycle verbs the server supports",
		about:   "Show sandbox lifecycle capabilities supported by the connected Shard server.",
		flags:   []flagHelp{formatTableHelp},
		notes: []note{para(
			"Lists all eight verbs, each true or false for the server's provider.",
			"Token scopes and sandbox states never change the answer.",
		)},
		examples: []string{"shard capabilities", "shard capabilities --format json"},
	},
	"info": {
		usage:   []string{"info [OPTIONS]"},
		summary: "show available providers and the default for this host",
		flags:   []flagHelp{formatTableHelp},
		notes: []note{
			para(
				"Works without an active daemon.",
				"Use 'shard daemon status' to see the provider the current daemon uses.",
			),
			hostOnlyNote,
		},
		examples: []string{"shard info", "shard info --format json"},
	},
	"serve": {
		usage:   []string{"serve [OPTIONS]"},
		summary: "start an HTTP API server with token authentication",
		flags: []flagHelp{
			{"--listen <address>", "listen address", serve.DefaultListen},
			signingKeyHelp,
		},
		notes: []note{
			para("The local daemon must be active.", "Use 'shard tokens mint' to create API tokens."),
			para("Use an HTTPS proxy or tunnel for public access.", "HTTP is suitable for local access or an encrypted VPN."),
			signingKeyNote,
			hostOnlyNote,
		},
		examples: []string{"shard serve"},
	},
	"tokens": {
		usage:   []string{"tokens COMMAND [OPTIONS] [ARGS...]"},
		summary: "create, list and revoke API tokens",
		notes: []note{para(
			"mint, list and revoke run only on the daemon host and refuse --remote.",
			"scopes follows --remote and "+client.RemoteEnv+".",
		)},
	},
	"tokens mint": {
		usage:   []string{"tokens mint [OPTIONS]"},
		summary: "create an API token",
		flags: []flagHelp{
			{"--name <name>", "token owner or purpose; required", ""},
			{"--duration <duration>", "token lifetime; default no expiry", ""},
			{"--scopes <list>", "permissions, separated by commas; default all permissions", ""},
			signingKeyHelp,
			formatJSONHelp,
		},
		notes: []note{
			para("Run shard tokens scopes to list available scopes."),
			signingKeyNote,
			para("The response includes the API token.", "Use its 'token' value as "+client.APIKeyEnv+"."),
			hostOnlyNote,
		},
		examples: []string{
			"shard tokens mint --name build-agent --duration 24h",
			"shard tokens mint --name reader --duration 24h --scopes sandbox:read",
		},
	},
	"tokens list": {
		usage:   []string{"tokens list [OPTIONS]"},
		summary: "list API tokens and their status",
		flags:   []flagHelp{registryKeyHelp, formatTableHelp},
		notes: []note{
			para(
				"Table columns: ID, NAME, ISSUED, EXPIRES, SCOPES and STATUS.",
				"Does not create a signing key.",
			),
			hostOnlyNote,
		},
		examples: []string{"shard tokens list", "shard tokens list --format json"},
	},
	"tokens revoke": {
		usage:   []string{"tokens revoke [OPTIONS] TOKEN", "tokens revoke [OPTIONS] --name NAME"},
		summary: "revoke an API token",
		args:    []row{{"TOKEN", "token ID from 'shard tokens list'"}},
		flags: []flagHelp{
			{"--name <name>", "revoke all tokens with this name", ""},
			registryKeyHelp,
		},
		notes:    []note{para("Revoked tokens are rejected on subsequent requests."), hostOnlyNote},
		examples: []string{"shard tokens revoke 0123456789abcdef", "shard tokens revoke --name build-agent"},
	},
	"tokens scopes": {
		usage:    []string{"tokens scopes [OPTIONS]"},
		summary:  "list available token scopes",
		flags:    []flagHelp{formatTableHelp},
		examples: []string{"shard tokens scopes", "shard tokens scopes --format json"},
	},
	"version": {
		usage:    []string{"version [OPTIONS]"},
		summary:  "show the client and daemon versions",
		flags:    []flagHelp{formatTableHelp},
		notes:    []note{para("Use 'shard --version' to show only the client version.")},
		examples: []string{"shard version", "shard version --format json"},
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
	sections := []string{strings.Join(usage, "\n"), wrap("", 0, h.opening())}

	if key == "" {
		sections = append(sections, topLevel()...)
	}
	if len(cmd.subs) > 0 {
		sections = append(sections, "Commands:\n"+columns(subRows(cmd)))
	}
	if len(h.args) > 0 {
		sections = append(sections, "Arguments:\n"+columns(h.args))
	}
	if len(h.flags) > 0 {
		heading := "Options:\n"
		if key == "" {
			heading = "Global options:\n"
		}
		sections = append(sections, heading+columns(flagRows(h.flags)))
	}
	if len(h.env) > 0 {
		sections = append(sections, "Environment variables:\n"+columns(h.env))
	}
	for _, n := range h.notes {
		sections = append(sections, n.render())
	}
	if key == "" || isCommand && cmd.run == nil {
		sections = append(sections, wrap("", 0, "Run '"+strings.Join(strings.Fields("shard "+key+" COMMAND --help"), " ")+"' for options and examples."))
	}
	if len(h.examples) > 0 {
		heading := "Examples:\n  "
		if len(h.examples) == 1 {
			heading = "Example:\n  "
		}
		sections = append(sections, heading+strings.Join(h.examples, "\n  "))
	}

	return strings.Join(sections, "\n\n")
}

// opening is the sentence a help starts with: its about, or else its summary as a sentence.
func (h verbHelp) opening() string {
	if h.about != "" {
		return h.about
	}
	first, size := utf8.DecodeRuneInString(h.summary)

	return string(unicode.ToUpper(first)) + h.summary[size:] + "."
}

// topLevel is the verb groups, one line per verb or noun with its summary.
func topLevel() []string {
	var all []row
	groups := make([][]row, 0, len(verbGroups))
	for _, group := range verbGroups {
		var rows []row
		for _, name := range group.verbs {
			rows = append(rows, row{name, helps[name].summary})
		}
		groups = append(groups, rows)
		all = append(all, rows...)
	}

	// One width for every group, so the summaries line up down the whole screen.
	width := widest(all)
	sections := make([]string, 0, len(verbGroups))
	for i, group := range verbGroups {
		sections = append(sections, group.title+":\n"+table(groups[i], width))
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

// render prints a paragraph as it stands, or a titled note indented under its title, the table before the lines.
func (n note) render() string {
	if n.title == "" {
		return wrapLines("", 0, n.lines)
	}

	var parts []string
	if len(n.rows) > 0 {
		parts = append(parts, columns(n.rows))
	}
	if len(n.lines) > 0 {
		parts = append(parts, wrapLines("  ", 2, n.lines))
	}

	return n.title + ":\n" + strings.Join(parts, "\n\n")
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

// wrap wraps each line of text on its own, so a newline in it starts a line indented by indent.
func wrap(lead string, indent int, text string) string {
	var lines []string
	for i, part := range strings.Split(text, "\n") {
		if i > 0 {
			lead = strings.Repeat(" ", indent)
		}
		lines = append(lines, wrapLine(lead, indent, part))
	}

	return strings.Join(lines, "\n")
}

// wrapLine appends text to lead a word at a time, and starts a line indented by indent wherever the next word would pass the width.
func wrapLine(lead string, indent int, text string) string {
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

// wrapLines wraps each line on its own after lead and the indent the line starts with, and keeps an empty line empty.
func wrapLines(lead string, indent int, lines []string) string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		text := strings.TrimLeft(line, " ")
		if text == "" {
			out = append(out, "")

			continue
		}
		pad := line[:len(line)-len(text)]
		out = append(out, wrap(lead+pad, indent+len(pad), text))
	}

	return strings.Join(out, "\n")
}

// flagName is the name a spelled flag parses as: --memory <size> is memory, and -f, --follow is follow.
func flagName(spell string) string {
	name, _, _ := strings.Cut(strings.TrimLeft(longSpell(spell), "-"), " ")

	return name
}

// placeholder is what a spelled flag takes after its name, as <size>, or nothing for a bool.
func placeholder(spell string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(longSpell(spell), "-"), flagName(spell)))
}

// longSpell drops the alias a spell leads with, so -f, --follow reads as --follow.
func longSpell(spell string) string {
	if _, long, ok := strings.Cut(spell, ", "); ok {
		return long
	}

	return spell
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
