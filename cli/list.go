package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
)

// listOptions is one parsed shard list invocation.
type listOptions struct {
	all    bool
	format outputFormat
}

// list asks the daemon and nothing else: the state it lists is what the last verb left in the record.
func (a App) list(ctx context.Context, args []string) error {
	opts, err := parseList(args)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}

	// Every sandbox is asked for, so a list without --all can say how many stopped ones it leaves out.
	result, err := c.ListSandboxes(ctx, true)
	if err != nil {
		return err
	}
	shown := result.Sandboxes
	if !opts.all {
		shown = slices.DeleteFunc(slices.Clone(shown), func(sb client.Sandbox) bool { return sb.State == models.StateStopped })
	}

	// The daemon answers with both: the sandboxes it read are printed, and the ones it could not are the exit.
	if err := a.writeList(opts.format, shown, time.Now()); err != nil {
		return err
	}
	if err := a.noteHidden(opts.format, len(result.Sandboxes)-len(shown)); err != nil {
		return err
	}

	if len(result.Warnings) == 0 {
		return nil
	}

	return errors.New(strings.Join(result.Warnings, "\n"))
}

// noteHidden says on stderr how many stopped sandboxes the table left out; JSON is for a script, which asks with --all.
func (a App) noteHidden(format outputFormat, hidden int) error {
	if hidden == 0 || format == formatJSON || a.Err == nil {
		return nil
	}
	noun := "sandboxes"
	if hidden == 1 {
		noun = "sandbox"
	}
	if _, err := fmt.Fprintf(a.Err, "%d stopped %s; shard list --all\n", hidden, noun); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func (a App) writeList(format outputFormat, sandboxes []client.Sandbox, now time.Time) error {
	if format != formatJSON {
		return writeTable(a.Out, sandboxes, now)
	}

	return writeJSON(a.Out, sandboxes)
}

func writeTable(w io.Writer, sandboxes []client.Sandbox, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tSTATE\tUPTIME\tRESTART\tPOLICY")

	for _, sb := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", sb.ID, orDash(sb.Name), sb.Image, state(sb), uptime(sb, now), restart(sb), orDash(sb.Policy))
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

// state carries the reason a sandbox nobody stopped is stopped, or the exit of an entrypoint whose
// still-running sandbox outlived it, which is the one an operator asks about.
func state(sb client.Sandbox) string {
	if sb.State == models.StateRunning && sb.ExitStatus != nil {
		return fmt.Sprintf("%s (exited %d)", sb.State, sb.ExitStatus.Code)
	}
	if sb.StoppedReason != "" {
		return fmt.Sprintf("%s (%s)", sb.State, sb.StoppedReason)
	}

	return string(sb.State)
}

// restart is the entrypoint policy the sandbox asked for, and how much of its cap has been spent on it.
func restart(sb client.Sandbox) string {
	if sb.Restart == nil {
		return "-"
	}

	return spent(string(sb.Restart.Policy), sb.Restart.Count, sb.Restart.Retries, sb.Restart.GaveUp)
}

// spent is a policy with its count beside it once a start again happened, its limit when it has one, and the give-up.
func spent(policy string, count, limit int, gaveUp bool) string {
	if count != 0 && limit != 0 {
		policy = fmt.Sprintf("%s %d/%d", policy, count, limit)
	}
	if count != 0 && limit == 0 {
		policy = fmt.Sprintf("%s %d", policy, count)
	}
	if gaveUp {
		return policy + " gave up"
	}

	return policy
}

// uptime is how long the sandbox has run since its last start or resume; only a live one is up.
func uptime(sb client.Sandbox, now time.Time) string {
	if !sb.State.Live() {
		return "-"
	}

	// A record an older daemon created and never started again holds no StartedAt.
	since := sb.StartedAt
	if since.IsZero() {
		since = sb.CreatedAt
	}

	return short(now.Sub(since).Truncate(time.Second))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func parseList(args []string) (listOptions, error) {
	var opts listOptions

	flags := newFlags("list")
	flags.BoolVar(&opts.all, "all", false, "")
	format := addFormatFlag(flags, formatTable)

	if err := parseVerb(flags, args); err != nil {
		return listOptions{}, err
	}
	opts.format = *format

	if rest := flags.Args(); len(rest) != 0 {
		return listOptions{}, fmt.Errorf("list takes no arguments, got %s", gotArgs(rest))
	}

	return opts, nil
}
