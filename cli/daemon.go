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
	if len(args) == 1 && args[0] == "status" {
		return a.daemonStatus(ctx)
	}
	if len(args) != 0 {
		return fmt.Errorf("daemon takes no argument, or status, got %s", strings.Join(args, " "))
	}

	return daemon.Run(ctx, daemon.Config{
		Version:     a.Version,
		Root:        a.Root,
		Out:         a.Out,
		Insecure:    a.Insecure,
		PullTimeout: a.Timeout,
		InitPath:    a.InitPath,
		Provider:    a.Provider,
	})
}

// daemonStatus prints what the daemon on the socket says about itself, one field per line.
func (a App) daemonStatus(ctx context.Context) error {
	d, err := a.client().Daemon(ctx)
	if err != nil {
		return err
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

	return nil
}
