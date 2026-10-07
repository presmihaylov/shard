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
	"github.com/presmihaylov/shard/services/sandbox"
)

// listOptions is one parsed shard list invocation.
type listOptions struct {
	all    bool
	format outputFormat
}

// formatIDs is what --quiet prints, one id per line for a script to hand to another verb; --format never takes it.
const formatIDs outputFormat = "ids"

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
		// A start the daemon owes after an OOM kill keeps the sandbox in view, as the daemon's own list does.
		shown = slices.DeleteFunc(slices.Clone(shown), func(sb client.Sandbox) bool {
			return sb.State == models.StateStopped && (sb.OOM == nil || sb.OOM.RestartAt.IsZero())
		})
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

// noteHidden says on stderr how many stopped sandboxes the table left out; any other output is for a script, which asks with --all.
func (a App) noteHidden(format outputFormat, hidden int) error {
	if hidden == 0 || format != formatTable || a.Err == nil {
		return nil
	}
	noun := "sandboxes"
	if hidden == 1 {
		noun = "sandbox"
	}
	note, err := a.hinted(fmt.Sprintf("%d stopped %s; shard list --all", hidden, noun))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(a.Err, note); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func (a App) writeList(format outputFormat, sandboxes []client.Sandbox, now time.Time) error {
	if format == formatIDs {
		return writeIDs(a.Out, sandboxes)
	}
	if format != formatJSON {
		return writeTable(a.Out, sandboxes, now)
	}

	return writeJSON(a.Out, sandboxes)
}

func writeIDs(w io.Writer, sandboxes []client.Sandbox) error {
	for _, sb := range sandboxes {
		if _, err := fmt.Fprintln(w, sb.ID); err != nil {
			return fmt.Errorf("write the output: %w", err)
		}
	}

	return nil
}

func writeTable(w io.Writer, sandboxes []client.Sandbox, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tSTATE\tUPTIME\tPROCESSES\tPOLICY")

	for _, sb := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", sb.ID, orDash(sb.Name), sb.Image, state(sb, now), uptime(sb, now), processes(sb), orDash(sb.Policy))
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

// state adds why a sandbox nobody stopped is stopped, and the memory kills it was started again after.
func state(sb client.Sandbox, now time.Time) string {
	var notes []string
	// The kill count says the memory ran out, so the reason would say it twice.
	if sb.StoppedReason != "" && (sb.OOM == nil || sb.StoppedReason != sandbox.OOMKilledReason) {
		notes = append(notes, sb.StoppedReason)
	}
	if sb.OOM != nil {
		notes = append(notes, outOfMemory(*sb.OOM, now))
	}
	if len(notes) == 0 {
		return string(sb.State)
	}

	return fmt.Sprintf("%s (%s)", sb.State, strings.Join(notes, "; "))
}

// outOfMemory counts the kills, and says when the daemon starts the sandbox again if it still owes that.
func outOfMemory(oom client.OOM, now time.Time) string {
	times := fmt.Sprintf("%d times", oom.Kills)
	if oom.Kills == 1 {
		times = "once"
	}
	note := "ran out of memory " + times
	if oom.RestartAt.IsZero() {
		return note
	}
	if !oom.RestartAt.After(now) {
		return note + ", starts again now"
	}

	return note + ", starts again in " + short(oom.RestartAt.Sub(now).Round(time.Second))
}

// processes is how many of the sandbox's named processes run, of all it has; shard ps names them.
func processes(sb client.Sandbox) string {
	if len(sb.Processes) == 0 {
		return "-"
	}

	up := 0
	for _, p := range sb.Processes {
		if !p.Status.State.Ended() {
			up++
		}
	}

	return fmt.Sprintf("%d/%d", up, len(sb.Processes))
}

// uptime is how long the sandbox has run since its last start, which a resume keeps; only a live one is up.
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
	var quiet bool
	flags.BoolVar(&quiet, "q", false, "")
	flags.BoolVar(&quiet, "quiet", false, "")

	if err := parseVerb(flags, args); err != nil {
		return listOptions{}, err
	}
	opts.format = *format
	if quiet && formatSet(flags) {
		return listOptions{}, errors.New("list takes --quiet or --format, not both")
	}
	if quiet {
		opts.format = formatIDs
	}

	if rest := flags.Args(); len(rest) != 0 {
		return listOptions{}, fmt.Errorf("list takes no arguments, got %s", gotArgs(rest))
	}

	return opts, nil
}
