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
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/presmihaylov/shard/services/client"
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

// App is the wiring one shard process needs.
type App struct {
	Version string
	// Root defaults to DefaultRoot when empty.
	Root string
	Out  io.Writer
	// Err carries warnings that must not fail the command. It defaults to nowhere.
	Err io.Writer
	// InitPath is the host path of the guest supervisor. It defaults to the environment when empty, and stays empty on a Mac.
	InitPath string
	// Remote is the shard serve a verb speaks to instead of the socket, as https://box:2376.
	Remote string
	// TokenFile holds the bearer token that serve checks, and CAFile the CA certificate that signed the serve certificate.
	TokenFile string
	CAFile    string

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
type printExit struct {
	text string
	// flags names every flag the verb parses, so a test holds its help to them.
	flags []string
}

func (p printExit) Error() string { return p.text }

// newFlags is the flag set of one verb. It prints nothing itself: its help and its errors come from helps.
func newFlags(verb string) *flag.FlagSet {
	flags := flag.NewFlagSet("shard "+verb, flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	return flags
}

// parseVerb parses a verb's flags, turns --help into a printExit Run prints, exit 0, and words a flag error as the help does.
func parseVerb(flags *flag.FlagSet, args []string) error {
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		var names []string
		flags.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })

		return printExit{text: helpText(helpKey(flags)), flags: names}
	}
	if err != nil {
		return flagError(flags, err)
	}

	return nil
}

// parseArgs parses a verb that takes no flags, so --help still prints its help and any other flag is refused.
func parseArgs(verb string, args []string) ([]string, error) {
	flags := newFlags(verb)
	if err := parseVerb(flags, args); err != nil {
		return nil, err
	}

	return flags.Args(), nil
}

// helpKey is the key in helps of the verb a flag set parses: shard image rm is image rm, and shard alone the top level.
func helpKey(flags *flag.FlagSet) string {
	return strings.TrimPrefix(strings.TrimPrefix(flags.Name(), "shard"), " ")
}

// The flag package words its refusals in Go's one-dash spelling; these read them back.
var (
	undefinedFlag = regexp.MustCompile(`^flag provided but not defined: -(.+)$`)
	valuelessFlag = regexp.MustCompile(`^flag needs an argument: -(.+)$`)
	invalidValue  = regexp.MustCompile(`^invalid (?:boolean )?value (".*") for (?:flag )?-([^:]+): (.+)$`)
)

// flagError says what the flag package refused the way the help spells it: the flag with its dashes, and the unit its value wants.
func flagError(flags *flag.FlagSet, err error) error {
	msg := err.Error()
	if m := undefinedFlag.FindStringSubmatch(msg); m != nil {
		return fmt.Errorf("unknown flag %s; run %s --help", dashed(m[1]), flags.Name())
	}
	if m := valuelessFlag.FindStringSubmatch(msg); m != nil {
		if want := wanted(flags, m[1]); want != "" {
			return fmt.Errorf("%s needs a value: %s", dashed(m[1]), want)
		}

		return fmt.Errorf("%s needs a value", dashed(m[1]))
	}
	if m := invalidValue.FindStringSubmatch(msg); m != nil {
		value, name, reason := m[1], m[2], m[3]
		// The flag package's own two reasons say nothing of the unit; a Value of ours already says what it wants.
		if want := wanted(flags, name); want != "" && (reason == "parse error" || reason == "value out of range") {
			reason = "want " + want
		}

		return fmt.Errorf("invalid value %s for %s: %s", value, dashed(name), reason)
	}

	return err
}

// wanted is what a flag's value must be: true or false for a bool, else what its placeholder in the help stands for.
func wanted(flags *flag.FlagSet, name string) string {
	if f := flags.Lookup(name); f != nil {
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			return "true or false"
		}
	}
	for _, f := range helps[helpKey(flags)].flags {
		if flagName(f.spell) == name {
			return wants[placeholder(f.spell)]
		}
	}

	return ""
}

// command is one word the dispatcher takes: a verb it runs, a noun whose subcommands it runs, or both, as daemon is.
type command struct {
	name string
	// aliases are spellings the dispatcher also takes and the help never lists.
	aliases []string
	run     func(App, context.Context, []string) error
	subs    []command
}

// commands is every word the dispatcher takes; helps holds what each prints, and a test holds the two to each other.
func commands() []command {
	return []command{
		{name: "create", run: App.create},
		{name: "exec", run: App.exec},
		{name: "ls", run: App.ls},
		{name: "logs", run: App.logs},
		{name: "inspect", run: App.inspect},
		{name: "stop", run: App.stop},
		{name: "start", run: App.start},
		{name: "rm", run: App.remove},
		{name: "pause", run: App.pause},
		{name: "resume", run: App.resume},
		{name: "fork", run: App.fork},
		{name: "clone", run: App.clone},
		{name: "cp", run: App.cp},
		{name: "pull", run: App.pull},
		{name: "image", subs: []command{
			{name: "ls", aliases: []string{"list"}, run: App.imageList},
			{name: "rm", aliases: []string{"remove"}, run: App.imageRemove},
			{name: "prune", run: App.imagePrune},
		}},
		{name: "secret", subs: []command{
			{name: "set", run: App.secretSet},
			{name: "ls", aliases: []string{"list"}, run: App.secretList},
			{name: "rm", aliases: []string{"remove"}, run: App.secretRemove},
			{name: "grant", run: App.secretGrant},
			{name: "ungrant", run: App.secretUngrant},
		}},
		{name: "policy", subs: []command{
			{name: "create", run: App.policyCreate},
			{name: "show", run: App.policyShow},
			{name: "ls", aliases: []string{"list"}, run: App.policyList},
			{name: "rm", aliases: []string{"remove"}, run: App.policyRemove},
			{name: "attach", run: App.policyAttach},
			{name: "detach", run: App.policyDetach},
		}},
		{name: "daemon", run: App.daemon, subs: []command{{name: "status", run: App.daemonStatus}}},
		{name: "info", run: App.info},
		{name: "serve", run: App.serve},
		{name: "tokens", subs: []command{
			{name: "mint", run: App.tokensMint},
			{name: "ls", aliases: []string{"list"}, run: App.tokensList},
			{name: "revoke", run: App.tokensRevoke},
		}},
		{name: "version", run: App.version},
	}
}

// find answers the command a word names, by its name or by an alias.
func find(cmds []command, word string) (command, bool) {
	for _, cmd := range cmds {
		if cmd.name == word || slices.Contains(cmd.aliases, word) {
			return cmd, true
		}
	}

	return command{}, false
}

// lookup answers the command a help key such as image ls names, and false for the top level.
func lookup(key string) (command, bool) {
	var found command
	cmds := commands()
	for word := range strings.FieldsSeq(key) {
		cmd, ok := find(cmds, word)
		if !ok {
			return command{}, false
		}
		found, cmds = cmd, cmd.subs
	}

	return found, key != ""
}

func names(cmds []command) []string {
	out := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		out = append(out, cmd.name)
	}

	return out
}

func (a App) run(ctx context.Context, args []string) error {
	args, err := a.parseGlobals(args)
	if err != nil {
		return err
	}

	if len(args) == 0 || args[0] == "help" {
		return a.print(helpText(""))
	}

	cmd, ok := find(commands(), args[0])
	if !ok {
		return fmt.Errorf("unknown command %q; run shard help", args[0])
	}

	return a.dispatch(ctx, cmd, args[1:])
}

// dispatch runs a verb, or the subcommand its first argument names; a noun with none names the ones it takes.
func (a App) dispatch(ctx context.Context, cmd command, args []string) error {
	if len(args) > 0 {
		if sub, ok := find(cmd.subs, args[0]); ok {
			return sub.run(a, ctx, args[1:])
		}
	}
	if cmd.run != nil {
		return cmd.run(a, ctx, args)
	}

	rest, err := parseArgs(cmd.name, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("%s takes a subcommand: %s", cmd.name, orList(names(cmd.subs)))
	}

	return fmt.Errorf("unknown %s subcommand %q; want %s", cmd.name, rest[0], orList(names(cmd.subs)))
}

// daemonFlags are the daemon's own flags, which came before the verb once and are refused there now.
var daemonFlags = []string{"timeout", "insecure-registry", "provider"}

// movedFlag takes a daemon flag given before the verb, and keeps the refusal that says where it goes.
type movedFlag struct {
	name    string
	refusal *error
}

func (m movedFlag) String() string { return "" }

func (m movedFlag) Set(value string) error {
	if *m.refusal == nil {
		*m.refusal = fmt.Errorf("--%s is a shard daemon flag: shard daemon --%s %s", m.name, m.name, value)
	}

	return nil
}

// parseGlobals takes the flags that precede the command and returns what is left.
func (a *App) parseGlobals(args []string) ([]string, error) {
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
	flags.BoolVar(&showVersion, "version", false, "")
	flags.StringVar(&a.Root, "root", a.Root, "")
	flags.StringVar(&a.Remote, "remote", a.Remote, "")
	flags.StringVar(&a.TokenFile, "token-file", a.TokenFile, "")
	flags.StringVar(&a.CAFile, "ca-file", a.CAFile, "")
	var moved error
	for _, name := range daemonFlags {
		flags.Var(movedFlag{name: name, refusal: &moved}, name, "")
	}

	if err := parseVerb(flags, args); err != nil {
		return nil, err
	}

	// --version answers before the root is checked, so it never fails.
	if showVersion {
		return nil, printExit{text: "client " + a.Version}
	}
	if moved != nil {
		return nil, moved
	}

	// The fallback is the flag default, so an explicit empty or relative --root still lands here.
	if !filepath.IsAbs(a.Root) {
		return nil, fmt.Errorf("--root must be an absolute path, got %q", a.Root)
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

// remoteClient reads the token and the certificate, so a bad one fails before the verb dials.
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

// client speaks to the daemon on the socket, or through --remote; a verb asks only after its flags parsed, so --help reads no token.
func (a App) client() (*client.Client, error) {
	if a.Remote != "" {
		return remoteClient(a.Remote, a.TokenFile, a.CAFile)
	}

	c := client.New(a.Root)
	if a.clientTimeout != 0 {
		c.Timeout = a.clientTimeout
	}

	return c, nil
}

// version prints this binary's line first, so it is on the screen even when no daemon answers.
func (a App) version(ctx context.Context, args []string) error {
	rest, err := parseArgs("version", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("version takes no argument, got %d", len(rest))
	}

	if err := a.print("client " + a.Version); err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	d, err := c.Version(ctx)
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
