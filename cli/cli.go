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
	"sync"
	"time"

	"github.com/presmihaylov/shard/services/client"
)

// DefaultRoot is where shard keeps everything on the box. The client owns it: its connect hint names the unit there only.
const DefaultRoot = client.DefaultRoot

// DefaultTimeout bounds one pull inside the daemon. Without it a registry that accepts and stalls pins it.
const DefaultTimeout = 30 * time.Minute

// DefaultInitPath is where make devbox-sync installs the guest supervisor on a Linux box; a Mac daemon carries its own.
const DefaultInitPath = "/usr/local/bin/shard-init"

// InitPathEnv overrides the guest supervisor the daemon uses. It is a property of the install, so it is no create flag.
const InitPathEnv = "SHARD_INIT_PATH"

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
	// Remote is the shard serve front, or the proxy in front of it, a verb speaks to instead of the socket, as https://shard.example.com.
	Remote string
	// Interrupts hands out the stop signals; nil, as a test builds, gives a verb none.
	Interrupts *Interrupts

	// clientTimeout bounds one daemon call. A test sets it; zero keeps the client's default.
	clientTimeout time.Duration

	// in is the terminal this shard process holds. A test replaces it: a pipe is not a terminal.
	in *os.File

	// plainWarned is shared by every copy of the App one run makes, so a verb that builds two clients warns once.
	plainWarned *sync.Once
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
		flags.VisitAll(func(f *flag.Flag) {
			if _, refused := f.Value.(refusal); !refused {
				names = append(names, f.Name)
			}
		})

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

// helpKey is the key in helps of the verb a flag set parses: shard image remove is image remove, and shard alone the top level.
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
		{name: "run", run: App.launch},
		{name: "exec", run: App.exec},
		{name: "list", aliases: []string{"ls"}, run: App.list},
		{name: "logs", run: App.logs},
		{name: "inspect", run: App.inspect},
		{name: "stop", run: App.stop},
		{name: "start", run: App.start},
		{name: "remove", aliases: []string{"rm"}, run: App.remove},
		{name: "pause", run: App.pause},
		{name: "resume", run: App.resume},
		{name: "fork", run: App.fork},
		{name: "cp", run: App.cp},
		{name: "snapshot", subs: []command{
			{name: "create", run: App.snapshotCreate},
			{name: "list", aliases: []string{"ls"}, run: App.snapshotList},
			{name: "inspect", run: App.snapshotInspect},
			{name: "remove", aliases: []string{"rm"}, run: App.snapshotRemove},
		}},
		{name: "pull", run: App.pull},
		{name: "image", subs: []command{
			{name: "list", aliases: []string{"ls"}, run: App.imageList},
			{name: "remove", aliases: []string{"rm"}, run: App.imageRemove},
			{name: "prune", run: App.imagePrune},
		}},
		{name: "secret", subs: []command{
			{name: "set", run: App.secretSet},
			{name: "list", aliases: []string{"ls"}, run: App.secretList},
			{name: "remove", aliases: []string{"rm"}, run: App.secretRemove},
			{name: "grant", run: App.secretGrant},
			{name: "ungrant", run: App.secretUngrant},
		}},
		{name: "policy", subs: []command{
			{name: "create", run: App.policyCreate},
			{name: "show", run: App.policyShow},
			{name: "list", aliases: []string{"ls"}, run: App.policyList},
			{name: "remove", aliases: []string{"rm"}, run: App.policyRemove},
			{name: "attach", run: App.policyAttach},
			{name: "detach", run: App.policyDetach},
			{name: "logs", run: App.policyLogs},
		}},
		{name: "capabilities", run: App.capabilities},
		{name: "daemon", run: App.daemon, subs: []command{{name: "status", run: App.daemonStatus}}},
		{name: "info", run: App.info},
		{name: "serve", run: App.serve},
		{name: "tokens", subs: []command{
			{name: "mint", run: App.tokensMint},
			{name: "list", aliases: []string{"ls"}, run: App.tokensList},
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

// lookup answers the command a help key such as image list names, and false for the top level.
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

// refusal is a flag a verb takes only to say which verb it belongs to, so its help never lists it.
type refusal interface{ refused() }

// movedFlag takes a daemon flag given before the verb, and keeps the refusal that says where it goes.
type movedFlag struct {
	name    string
	refusal *error
}

func (m movedFlag) String() string { return "" }

func (m movedFlag) refused() {}

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
	a.plainWarned = &sync.Once{}

	flags := flag.NewFlagSet("shard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var showVersion bool
	flags.BoolVar(&showVersion, "version", false, "")
	flags.StringVar(&a.Root, "root", a.Root, "")
	flags.StringVar(&a.Remote, "remote", a.Remote, "")
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

// fromEnv fills --remote from the environment; the client reads the key and the ca file, so they live in one place.
func (a *App) fromEnv() {
	if a.Remote == "" {
		a.Remote = os.Getenv(client.RemoteEnv)
	}
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
	// The key and the certificate are read here, so a bad one fails before the verb dials.
	if a.Remote != "" {
		c, err := client.NewRemoteFromEnv(a.Remote)
		if err != nil {
			return nil, err
		}
		if c.Plain() && a.Err != nil {
			a.plainWarned.Do(func() { fmt.Fprintln(a.Err, plainWarning) })
		}

		return c, nil
	}

	c := client.New(a.Root)
	if a.clientTimeout != 0 {
		c.Timeout = a.clientTimeout
	}

	return c, nil
}

// localClient is the socket for a verb no front forwards, so a remote target fails here, before any dial, and never as a bare 403.
func (a App) localClient(verb string) (*client.Client, error) {
	if a.Remote != "" {
		return nil, fmt.Errorf("shard %s runs on the daemon host only, over its socket, and cannot reach %s; unset --remote and %s to run it there", verb, a.Remote, client.RemoteEnv)
	}

	return a.client()
}

// version prints this binary's line first, so it is on the screen even when no daemon answers.
func (a App) version(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("version", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("version takes no argument, got %d", len(rest))
	}
	// One JSON value needs the daemon's answer, so with no daemon it writes nothing.
	if format == formatJSON {
		daemonVersion, err := a.daemonVersion(ctx)
		if err != nil {
			return err
		}

		return writeJSON(a.Out, versionView{Client: a.Version, Daemon: daemonVersion, Shim: shimState()})
	}

	if err := a.print("client " + a.Version); err != nil {
		return err
	}

	daemonVersion, err := a.daemonVersion(ctx)
	if err != nil {
		return err
	}
	if err := a.print("daemon " + daemonVersion); err != nil {
		return err
	}
	if line := shimLine(); line != "" {
		return a.print(line)
	}

	return nil
}

func (a App) daemonVersion(ctx context.Context) (string, error) {
	c, err := a.client()
	if err != nil {
		return "", err
	}

	d, err := c.Version(ctx)
	if err != nil {
		return "", err
	}

	return d.Version, nil
}

// warn reports something the operator should know that is not a reason to fail the command.
func (a App) warn(message string) {
	if a.Err == nil {
		return
	}

	fmt.Fprintln(a.Err, "shard: warning:", message)
}

// plainWarning is the whole line an http remote prints, on stderr, so stdout stays the verb's own.
const plainWarning = "Warning: HTTP does not encrypt this connection. Use it only on localhost or through a trusted encrypted network."

func (a App) print(s string) error {
	if _, err := fmt.Fprintln(a.Out, s); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
