package cli

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// portAddOptions is what port add parsed: the sandbox, the forward, and whether it listens on every interface.
type portAddOptions struct {
	ref     string
	forward models.PortForward
}

func (a App) portAdd(ctx context.Context, args []string) error {
	opts, err := parsePortAdd(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	row, err := c.AddPort(ctx, opts.ref, opts.forward.HostPort, sandbox.PortRequest{GuestPort: opts.forward.GuestPort, Public: opts.forward.Public})
	if err != nil {
		return err
	}

	return a.printForward(row)
}

// printForward puts on stdout each address the forward is reachable on, and on stderr why there is none yet.
func (a App) printForward(row models.Port) error {
	name := cmp.Or(row.SandboxName, row.Sandbox)
	if !row.Listening && a.Err != nil {
		reason := fmt.Sprintf("sandbox %s is not running; host port %d forwards to its port %d once it starts", name, row.HostPort, row.GuestPort)
		if row.Error != "" {
			reason = fmt.Sprintf("host port %d does not listen: %s", row.HostPort, row.Error)
		}
		fmt.Fprintln(a.Err, "note: "+reason)
	}
	if row.Public && a.Err != nil {
		fmt.Fprintf(a.Err, "note: host port %d listens on every interface of this host; whoever reaches the host reaches port %d of sandbox %s\n", row.HostPort, row.GuestPort, name)
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	for _, on := range row.ReachableOn {
		fmt.Fprintf(w, "%s\t%s:%d\n", on.Interface, on.Address, row.HostPort)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func parsePortAdd(args []string) (portAddOptions, error) {
	var opts portAddOptions

	flags := newFlags("port add")
	flags.BoolVar(&opts.forward.Public, "public", false, "")

	rest, err := parseInterspersed(flags, args)
	if err != nil {
		return portAddOptions{}, err
	}
	if len(rest) != 2 {
		return portAddOptions{}, fmt.Errorf("port add takes a sandbox and a [HOST:]GUEST port, got %s: shard port add web 9000:8000", gotArgs(rest))
	}

	forward, err := parseForward(rest[1], "use --public to bind 0.0.0.0")
	if err != nil {
		return portAddOptions{}, err
	}
	opts.ref, opts.forward.HostPort, opts.forward.GuestPort = rest[0], forward.HostPort, forward.GuestPort

	return opts, nil
}

// parseInterspersed takes the flags on either side of the arguments, so --public reads as well after the ports as before them.
func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := parseVerb(flags, args); err != nil {
			return nil, err
		}
		left := flags.Args()
		if len(left) == 0 {
			return rest, nil
		}
		// After a -- every word is an argument, as a sandbox name that starts with - needs.
		if consumed := len(args) - len(left); consumed > 0 && args[consumed-1] == "--" {
			return append(rest, left...), nil
		}
		rest = append(rest, left[0])
		args = left[1:]
	}
}

// parseForward reads [HOST:]GUEST; an address is never typed, because a flag is the one way to 0.0.0.0.
func parseForward(text, hint string) (models.PortForward, error) {
	if strings.Contains(text, ".") || strings.Count(text, ":") > 1 {
		return models.PortForward{}, fmt.Errorf("a forward takes [HOST:]GUEST ports and no address, got %q: %s", text, hint)
	}

	host, guest, mapped := strings.Cut(text, ":")
	if !mapped {
		guest = host
	}
	hostPort, err := portNumber("host port", host)
	if err != nil {
		return models.PortForward{}, err
	}
	guestPort, err := portNumber("guest port", guest)
	if err != nil {
		return models.PortForward{}, err
	}

	return models.PortForward{HostPort: hostPort, GuestPort: guestPort}, nil
}

func portNumber(what, text string) (uint16, error) {
	n, err := strconv.ParseUint(text, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("the %s must be a number from 1 to 65535, got %q", what, text)
	}

	return uint16(n), nil
}

// portList is create's --port, repeatable: forwards on 127.0.0.1, since port add --public is the one way to 0.0.0.0.
type portList []models.PortForward

func (l *portList) String() string { return "" }

func (l *portList) Set(value string) error {
	forward, err := parseForward(value, "create forwards on 127.0.0.1; use shard port add --public to bind 0.0.0.0")
	if err != nil {
		return err
	}
	for _, have := range *l {
		if have.HostPort == forward.HostPort {
			return fmt.Errorf("host port %d is given twice; a host port carries to one guest port", forward.HostPort)
		}
	}

	*l = append(*l, forward)

	return nil
}

func (a App) portList(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("port list", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("port list takes at most one sandbox, got %s", gotArgs(rest))
	}
	ref := ""
	if len(rest) == 1 {
		ref = rest[0]
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	rows, err := c.ListPorts(ctx, ref)
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, nonNil(rows))
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "SANDBOX\tHOST\tGUEST\tVISIBILITY\tREACHABLE-ON")

	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", cmp.Or(row.SandboxName, row.Sandbox), row.HostPort, row.GuestPort, visibility(row.Public), reachableOn(row))
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func visibility(public bool) string {
	if public {
		return "public"
	}

	return "private"
}

// reachableOn is where a client connects while the port listens, or why it does not.
func reachableOn(row models.Port) string {
	if row.Error != "" && !row.Listening {
		return "not listening: " + row.Error
	}
	if len(row.ReachableOn) == 0 {
		return "-"
	}

	out := make([]string, 0, len(row.ReachableOn))
	for _, on := range row.ReachableOn {
		out = append(out, on.Address+":"+strconv.Itoa(int(row.HostPort)))
	}

	return strings.Join(out, ", ")
}

func (a App) portRemove(ctx context.Context, args []string) error {
	rest, err := parseArgs("port remove", args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return fmt.Errorf("port remove takes a sandbox and a host port, got %s: shard port remove web 9000", gotArgs(rest))
	}
	hostPort, err := portNumber("host port", rest[1])
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	if err := c.RemovePort(ctx, rest[0], hostPort); err != nil {
		return err
	}

	return a.print(rest[1])
}
