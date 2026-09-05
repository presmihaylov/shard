package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/secret"
)

// maxSecretBytes bounds what set reads, so a stray redirect of a disk image does not become a secret.
const maxSecretBytes = 64 << 10

func (a App) secret(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("secret takes a subcommand: set, ls, rm, grant or ungrant")
	}

	switch args[0] {
	case "set":
		return a.secretSet(ctx, args[1:])
	case "ls", "list":
		return a.secretList(args[1:])
	case "rm", "remove":
		return a.secretRemove(ctx, args[1:])
	case "grant":
		return a.secretGrant(ctx, args[1:], true)
	case "ungrant":
		return a.secretGrant(ctx, args[1:], false)
	}

	return fmt.Errorf("unknown secret subcommand %q; want set, ls, rm, grant or ungrant", args[0])
}

// secretSetOptions is one parsed shard secret set invocation.
type secretSetOptions struct {
	name         string
	destinations []string
	headers      []secret.Header
	match        *secret.Match
}

// secretSet reads the value from stdin, so it lands in no shell history and no process listing.
func (a App) secretSet(_ context.Context, args []string) error {
	opts, err := parseSecretSet(args)
	if err != nil {
		return err
	}

	d := a.deps()

	if pty.IsTerminal(d.stdin()) {
		return errors.New("secret set reads the value from stdin: pipe it in, as in printf '%s' \"$TOKEN\" | shard secret set --to <host> NAME")
	}

	value, err := readSecretValue(d.stdin())
	if err != nil {
		return err
	}

	store, err := d.secrets()
	if err != nil {
		return err
	}

	up := secret.Update{Destinations: opts.destinations, Headers: opts.headers, Match: opts.match, Holders: func() ([]string, error) { return holders(d, opts.name) }}

	sec, err := store.Set(opts.name, value, up)
	if err != nil {
		return err
	}

	return a.print(sec.Name)
}

// readSecretValue takes the whole of stdin less one trailing newline, which is what echo and a
// heredoc add and no secret carries. A second newline stays: it is then part of the value.
func readSecretValue(in io.Reader) (string, error) {
	blob, err := io.ReadAll(io.LimitReader(in, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read the secret value from stdin: %w", err)
	}
	if len(blob) > maxSecretBytes {
		return "", fmt.Errorf("the secret value is longer than %d bytes, and no credential is", maxSecretBytes)
	}

	value := strings.TrimSuffix(string(blob), "\n")
	if value == "" {
		return "", errors.New("the secret value is empty: pipe it into stdin, as in printf '%s' \"$TOKEN\" | shard secret set --to <host> NAME")
	}

	return value, nil
}

func parseSecretSet(args []string) (secretSetOptions, error) {
	var opts secretSetOptions

	flags := flag.NewFlagSet("shard secret set", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var((*hostList)(&opts.destinations), "to", "a host the value may go to, repeatable")
	flags.Var((*headerList)(&opts.headers), "header", "a header the proxy sets on a matched request, as 'Name: template' with {value}, repeatable")
	flags.Var(matchFlag{match: &opts.match}, "match", "a condition a request must meet to get the headers: path=, method=, query= or header=, repeatable")

	if err := flags.Parse(args); err != nil {
		return secretSetOptions{}, fmt.Errorf("parse the secret set flags: %w", err)
	}

	rest := flags.Args()
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return secretSetOptions{}, errors.New("secret set takes its flags before the name: shard secret set --to <host> <NAME>")
	}
	if len(rest) != 1 {
		return secretSetOptions{}, fmt.Errorf("secret set takes one name, got %d", len(rest))
	}

	opts.name = rest[0]

	return opts, nil
}

func (a App) secretList(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("secret ls takes no arguments, got %d", len(args))
	}

	store, err := a.deps().secrets()
	if err != nil {
		return err
	}

	// The readable secrets are listed before the error, so one broken file does not hide the rest.
	secrets, listErr := store.List()

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESTINATIONS\tPLACEHOLDER\tUPDATED")

	for _, sec := range secrets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sec.Name, strings.Join(sec.Destinations, ","), sec.MockValue, humanAge(sec.UpdatedAt))
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return listErr
}

// secretGrant hands a sandbox the placeholder of a stored secret after the create, or takes it
// back. Only the bundle and the record change, so the next start picks both up.
func (a App) secretGrant(ctx context.Context, args []string, attach bool) (err error) {
	verb := "grant"
	if !attach {
		verb = "ungrant"
	}

	if len(args) != 2 {
		return fmt.Errorf("secret %s takes a sandbox id and a secret name, got %d arguments", verb, len(args))
	}

	d := a.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	id, err := repo.Resolve(args[0])
	if err != nil {
		return err
	}

	release, err := repo.Hold(ctx, id)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()

	sb, err := repo.Get(id)
	if err != nil {
		return err
	}

	// A running guest holds its environment live, and a snapshot holds the paused one past this edit.
	if sb.State != models.StateCreated && sb.State != models.StateStopped {
		return fmt.Errorf("sandbox %s is %s: secret %s takes a created or stopped sandbox, stop it first", id, sb.State, verb)
	}

	dir, err := repo.Dir(id)
	if err != nil {
		return err
	}

	bnd, err := bundle.Open(dir)
	if err != nil {
		return err
	}

	if attach {
		if err := grantSecretTo(d, repo, bnd, sb, args[1]); err != nil {
			return err
		}
	}
	if !attach {
		if err := ungrantSecretFrom(repo, bnd, sb, args[1]); err != nil {
			return err
		}
	}

	return a.print(id)
}

// grantSecretTo is what create --secret does, done late: the placeholder into the bundle env, the
// proxy CA into the writable layer, and the grant into the record.
func grantSecretTo(d *deps, repo sandboxRepo, bnd bundle.Bundle, sb models.Sandbox, name string) error {
	if slices.Contains(sb.Secrets, name) {
		return fmt.Errorf("sandbox %s already holds secret %s", sb.ID, name)
	}

	store, err := d.secrets()
	if err != nil {
		return err
	}

	sec, err := store.Get(name)
	if err != nil {
		return err
	}

	// A sandbox created unfronted never trusted the proxy CA, and its next start is fronted.
	px, err := d.proxy()
	if err != nil {
		return err
	}
	ca, err := px.CA()
	if err != nil {
		return err
	}
	if err := bnd.TrustProxy(ca); err != nil {
		return err
	}

	if err := bnd.SetEnv(name, sec.MockValue); err != nil {
		return err
	}

	return repo.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.Secrets = append(sb.Secrets, name)

		return nil
	})
}

// ungrantSecretFrom leaves the proxy CA in place: trusting it is harmless once nothing fronts the sandbox.
func ungrantSecretFrom(repo sandboxRepo, bnd bundle.Bundle, sb models.Sandbox, name string) error {
	if !slices.Contains(sb.Secrets, name) {
		return fmt.Errorf("sandbox %s does not hold secret %s", sb.ID, name)
	}

	if err := bnd.RemoveEnv(name); err != nil {
		return err
	}

	return repo.Update(sb.ID, func(sb *models.Sandbox) error {
		sb.Secrets = slices.DeleteFunc(sb.Secrets, func(n string) bool { return n == name })

		return nil
	})
}

// secretRemoveOptions is one parsed shard secret rm invocation.
type secretRemoveOptions struct {
	name  string
	force bool
}

func (a App) secretRemove(_ context.Context, args []string) error {
	opts, err := parseSecretRemove(args)
	if err != nil {
		return err
	}

	d := a.deps()

	store, err := d.secrets()
	if err != nil {
		return err
	}

	// A removed secret leaves a placeholder no request can redeem, so the sandboxes that hold it are named first.
	if !opts.force {
		if err := ungranted(d, opts.name); err != nil {
			return err
		}
	}

	// A file that does not decode is still one to remove, so only a missing one stops here.
	if _, err := store.Get(opts.name); errors.Is(err, secret.ErrNotFound) {
		return err
	}

	if err := store.Remove(opts.name); err != nil {
		return err
	}

	return a.print(opts.name)
}

// granted is the refusal a removal gets while a sandbox record still names the secret.
type granted struct {
	name  string
	users []string
}

func (e granted) Error() string {
	return fmt.Sprintf("secret %s is granted to sandbox %s: remove the sandbox first, or pass --force", e.name, strings.Join(e.users, ", "))
}

// ungranted refuses when a sandbox record names the secret. A stopped one counts: start hands it the placeholder again.
func ungranted(d *deps, name string) error {
	users, err := holders(d, name)
	if err != nil {
		return fmt.Errorf("cannot tell which sandboxes hold the secret: %w", err)
	}

	if len(users) != 0 {
		return granted{name: name, users: users}
	}

	return nil
}

// holders names the sandboxes whose record grants the secret.
func holders(d *deps, name string) ([]string, error) {
	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	sandboxes, unreadable := repo.List()
	// A record that does not read back may name the secret, so nothing can say it is free.
	if unreadable != nil {
		return nil, unreadable
	}

	var users []string
	for _, sb := range sandboxes {
		if slices.Contains(sb.Secrets, name) {
			users = append(users, sb.ID)
		}
	}

	return users, nil
}

func parseSecretRemove(args []string) (secretRemoveOptions, error) {
	var opts secretRemoveOptions

	flags := flag.NewFlagSet("shard secret rm", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.force, "force", false, "remove the secret even when a sandbox holds it")

	if err := flags.Parse(args); err != nil {
		return secretRemoveOptions{}, fmt.Errorf("parse the secret rm flags: %w", err)
	}

	rest := flags.Args()
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return secretRemoveOptions{}, errors.New("secret rm takes its flags before the name: shard secret rm --force <NAME>")
	}
	if len(rest) != 1 {
		return secretRemoveOptions{}, fmt.Errorf("secret rm takes one name, got %d", len(rest))
	}

	opts.name = rest[0]

	return opts, secret.ValidName(opts.name)
}

// headerList collects repeated --header flags, each spelled 'Name: template'.
type headerList []secret.Header

func (h *headerList) String() string { return "" }

func (h *headerList) Set(raw string) error {
	name, value, ok := strings.Cut(raw, ":")
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	if !ok || name == "" || value == "" {
		return fmt.Errorf("a header is 'Name: template', with {value} for the secret value, not %q", raw)
	}
	*h = append(*h, secret.Header{Name: name, Value: value})

	return nil
}

// matchFlag folds repeated --match conditions into one Match. Every condition must hold at once.
type matchFlag struct{ match **secret.Match }

func (f matchFlag) String() string { return "" }

func (f matchFlag) Set(raw string) error {
	dim, rest, found := strings.Cut(raw, "=")
	if !found || rest == "" {
		return fmt.Errorf("a match is '<dimension>=<condition>', not %q", raw)
	}
	if *f.match == nil {
		*f.match = &secret.Match{}
	}
	m := *f.match

	switch dim {
	case "path":
		if m.Path != "" {
			return errors.New("a set takes one path match")
		}
		m.Path = rest
	case "method":
		for method := range strings.SplitSeq(rest, ",") {
			m.Methods = append(m.Methods, strings.ToUpper(strings.TrimSpace(method)))
		}
	case "query":
		name, value, ok := strings.Cut(rest, "=")
		if !ok || name == "" {
			return fmt.Errorf("a query match is 'query=<name>=<value>', not %q", raw)
		}
		m.Query = append(m.Query, secret.Pair{Name: name, Value: value})
	case "header":
		name, value, ok := strings.Cut(rest, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return fmt.Errorf("a header match is 'header=<Name>: <value>', not %q", raw)
		}
		m.Headers = append(m.Headers, secret.Pair{Name: name, Value: strings.TrimSpace(value)})
	default:
		return fmt.Errorf("unknown match dimension %q; want path, method, query or header", dim)
	}

	return nil
}
