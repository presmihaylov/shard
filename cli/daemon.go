package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/services/daemon"
)

// daemon runs the resident process systemd starts: the background work, the API socket and the sandbox lifecycle.
func (a App) daemon(ctx context.Context, args []string) error {
	flags := newFlags("daemon")
	logPath := flags.String("log", "", "")
	provider := flags.String("provider", "", "")
	timeout := flags.Duration("timeout", DefaultTimeout, "")
	var insecure []string
	flags.Var((*hostList)(&insecure), "insecure-registry", "")

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("daemon takes no argument, or status, got %s", strings.Join(flags.Args(), " "))
	}

	return daemon.Run(ctx, daemon.Config{
		Version:     a.Version,
		Root:        a.Root,
		Out:         a.Out,
		Insecure:    insecure,
		PullTimeout: *timeout,
		InitPath:    a.InitPath,
		Provider:    *provider,
		LogPath:     *logPath,
	})
}

// daemonStatus prints what the daemon on the socket says about itself, one field per line.
func (a App) daemonStatus(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("daemon status", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("daemon status takes no argument, got %d", len(rest))
	}

	c, err := a.localClient("daemon status")
	if err != nil {
		return err
	}

	d, err := c.Daemon(ctx)
	if err != nil {
		return err
	}

	var backoff []string
	for _, t := range d.Tasks {
		if t.State == daemon.TaskBackoff {
			backoff = append(backoff, t.Name)
		}
	}

	if format == formatJSON {
		if err := writeJSON(a.Out, d); err != nil {
			return err
		}

		return backoffError(backoff)
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintf(w, "version\t%s\n", d.Version)
	fmt.Fprintf(w, "pid\t%d\n", d.PID)
	fmt.Fprintf(w, "started_at\t%s\n", d.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "socket\t%s\n", d.Socket)
	fmt.Fprintf(w, "provider\t%s\n", d.Provider)
	fmt.Fprintf(w, "pause\t%s\n", strconv.FormatBool(d.Capabilities.Pause))
	fmt.Fprintf(w, "resume\t%s\n", strconv.FormatBool(d.Capabilities.Resume))
	fmt.Fprintf(w, "fork\t%s\n", strconv.FormatBool(d.Capabilities.Fork))
	fmt.Fprintf(w, "plain_port\t%d\n", d.Proxy.PlainPort)
	fmt.Fprintf(w, "tls_port\t%d\n", d.Proxy.TLSPort)

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	if len(d.Tasks) == 0 {
		return nil
	}

	tw := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "\ntask\tstate\trestarts\tlast_error")
	for _, t := range d.Tasks {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", t.Name, t.State, t.Restarts, t.LastError)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return backoffError(backoff)
}

// backoffError fails a status whose task restarts in a loop: that is an unhealthy daemon, and the exit code says so to a script.
func backoffError(backoff []string) error {
	if len(backoff) == 0 {
		return nil
	}

	return fmt.Errorf("tasks in backoff: %s", strings.Join(backoff, ", "))
}
