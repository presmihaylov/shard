package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/services/client"
)

// sshDomain ends every host the printed config sends to a sandbox, and ssh-proxy drops it to find the sandbox.
const sshDomain = ".shard"

// sshValued is every option letter of OpenSSH's ssh that takes a value, so the word after one is its value and not a host.
const sshValued = "bceilmopBDEFIJLOPQRSwW"

// shellSafe is a word the shell reads as itself, so the ProxyCommand stays readable when nothing needs quoting.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@=+,-]+$`)

// sshOption is one ssh_config keyword and its value, quoted the way ssh reads it.
type sshOption struct{ key, value string }

// sshFlags is one parsed shard ssh invocation.
type sshFlags struct {
	id      string
	user    string
	options []string
	command []string
}

// ssh runs the ssh client in place of shard, pointed at the sandbox through ssh-proxy, so -L, scp's flags and the exit code are ssh's own.
func (a App) ssh(_ context.Context, args []string) error {
	opts, err := parseSSH(args)
	if err != nil {
		return err
	}

	path, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("shard ssh runs the OpenSSH client, and found no ssh on PATH; install it, or use shard shell: %w", err)
	}

	options, err := a.sshOptions()
	if err != nil {
		return err
	}

	// ssh keeps the first value an option gets, so these come before the caller's, and -- keeps a name that starts with - a host.
	argv := []string{"ssh"}
	for _, o := range options {
		argv = append(argv, "-o", o.key+"="+o.value)
	}
	argv = append(argv, "-l", opts.user)
	argv = append(argv, opts.options...)
	argv = append(argv, "--", opts.id+sshDomain)
	argv = append(argv, opts.command...)

	replace := a.replace
	if replace == nil {
		replace = syscall.Exec
	}
	if err := replace(path, argv, os.Environ()); err != nil {
		return fmt.Errorf("run %s: %w", path, err)
	}

	return nil
}

func parseSSH(args []string) (sshFlags, error) {
	var opts sshFlags

	flags := newFlags("ssh")
	flags.StringVar(&opts.user, "user", "root", "")

	if err := parseVerb(flags, args); err != nil {
		return sshFlags{}, err
	}
	if opts.user == "" {
		return sshFlags{}, errors.New("--user needs a user name, as --user root")
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return sshFlags{}, errors.New("ssh takes a sandbox id or name, as in shard ssh web1")
	}
	opts.id = strings.TrimSuffix(rest[0], sshDomain)

	options, command, err := splitSSH(rest[1:])
	if err != nil {
		return sshFlags{}, err
	}
	opts.options, opts.command = options, command

	return opts, nil
}

// splitSSH splits what follows the sandbox at --, and refuses a word ssh would take for the host, which would reach another sandbox.
func splitSSH(args []string) (options, command []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return args[:i], args[i+1:], nil
		case strings.HasPrefix(arg, "--"):
			return nil, nil, fmt.Errorf("%s goes before the sandbox, as in shard ssh %s SANDBOX; after it come ssh's own options", arg, arg)
		case len(arg) < 2 || arg[0] != '-':
			return nil, nil, fmt.Errorf("%q is no ssh option; put a command after --, as in shard ssh SANDBOX -- %s", arg, arg)
		}

		at := strings.IndexAny(arg[1:], sshValued) + 1
		if at == 0 {
			continue
		}
		if arg[at] == 'l' {
			return nil, nil, errors.New("pick the user with --user before the sandbox, as in shard ssh --user app SANDBOX")
		}
		if at == len(arg)-1 && i == len(args)-1 {
			return nil, nil, fmt.Errorf("ssh option -%c needs a value", arg[at])
		}
		if at == len(arg)-1 {
			i++
		}
	}

	return args, nil, nil
}

// sshConfig prints the ssh_config block that sends every SANDBOX.shard to its sandbox, for scp, rsync or an editor.
func (a App) sshConfig(_ context.Context, args []string) error {
	rest, err := parseArgs("ssh-config", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("ssh-config takes no arguments, got %s", gotArgs(rest))
	}

	options, err := a.sshOptions()
	if err != nil {
		return err
	}

	lines := []string{"Host *" + sshDomain}
	for _, o := range append(options, sshOption{"User", "root"}) {
		lines = append(lines, "  "+o.key+" "+o.value)
	}

	return a.print(strings.Join(lines, "\n"))
}

// sshProxy is the ProxyCommand of a SANDBOX.shard host: it pins the daemon's host key, then carries ssh's bytes over the API.
func (a App) sshProxy(ctx context.Context, args []string) error {
	rest, err := parseArgs("ssh-proxy", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("ssh-proxy takes the one host ssh connects to, as web1.shard, got %s", gotArgs(rest))
	}

	_, pin, err := a.sshPin()
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}

	conn, key, err := c.SSH(ctx, strings.TrimSuffix(rest[0], sshDomain))
	if err != nil {
		return err
	}
	// ssh checks the key against the pin once the daemon's first bytes arrive, so the pin is written before any of them.
	if err := pin.Pin(key); err != nil {
		return errors.Join(err, conn.Close())
	}

	return splice(conn, a.stdin(), a.Out)
}

// sshPin is the remote this invocation reaches, empty for the socket, and the known_hosts pin of that daemon.
func (a App) sshPin() (string, client.SSHPin, error) {
	saved, err := a.saved()
	if err != nil {
		return "", client.SSHPin{}, err
	}
	remote := cmp.Or(a.Remote, saved.Remote)

	pin, err := client.PinFor(os.Getenv, a.Root, remote)
	if err != nil {
		return "", client.SSHPin{}, err
	}

	return remote, pin, nil
}

// sshOptions is what points ssh at this invocation's daemon; ssh-config prints them and shard ssh passes them as -o, so the two agree.
func (a App) sshOptions() ([]sshOption, error) {
	remote, pin, err := a.sshPin()
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find this shard binary for ssh's ProxyCommand: %w", err)
	}
	remote, err = withoutUser(remote)
	if err != nil {
		return nil, err
	}

	// The command names its daemon outright, so a later SHARD_REMOTE or saved remote cannot send it past the pin.
	words := []string{self, "--remote", remote}
	if remote == "" {
		words = append(words, "--root", a.Root)
	}
	words = append(words, "ssh-proxy", "--")
	for _, word := range append(slices.Clone(words), pin.KnownHosts) {
		if strings.ContainsAny(word, "\r\n") {
			return nil, fmt.Errorf("cannot put %q in an ssh option: it holds a line break", word)
		}
	}

	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = shellQuote(word)
	}

	// ssh expands % in both values, and %n is the host as typed, which ssh-proxy takes.
	return []sshOption{
		{"ProxyCommand", strings.ReplaceAll(strings.Join(quoted, " "), "%", "%%") + " %n"},
		{"UserKnownHostsFile", configQuote(strings.ReplaceAll(pin.KnownHosts, "%", "%%"))},
		{"HostKeyAlias", pin.Alias},
		{"StrictHostKeyChecking", "yes"},
	}, nil
}

// withoutUser drops a user and password from remote, which the client never sends, so no secret lands in an ssh config.
func withoutUser(remote string) (string, error) {
	if remote == "" {
		return "", nil
	}
	parsed, err := url.Parse(remote)
	if err != nil {
		return "", fmt.Errorf("parse the remote %s: %w", client.Redacted(remote), err)
	}
	parsed.User = nil

	return parsed.String(), nil
}

// shellQuote is word as the shell that runs a ProxyCommand reads it.
func shellQuote(word string) string {
	if shellSafe.MatchString(word) {
		return word
	}

	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// configQuote is value as ssh_config reads one argument, which splits on blanks unless they are quoted.
func configQuote(value string) string {
	if !strings.ContainsAny(value, " \t\"'\\") {
		return value
	}

	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// splice carries bytes both ways until either side ends; a read of stdin still blocked ends with the process.
func splice(conn net.Conn, in io.Reader, out io.Writer) error {
	ended := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, in)
		ended <- endOf("send ssh's bytes to the sandbox", err)
	}()
	go func() {
		_, err := io.Copy(out, conn)
		ended <- endOf("pass the sandbox's bytes to ssh", err)
	}()

	err := <-ended
	closeErr := conn.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	}

	return errors.Join(err, closeErr)
}

// endOf is how one direction of a splice ended; a stream the other side already closed is the normal end of a session.
func endOf(what string, err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}

	return fmt.Errorf("%s: %w", what, err)
}
