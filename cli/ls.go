package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/models"
)

// lsOptions is one parsed shard ls invocation.
type lsOptions struct {
	all bool
}

// ls asks the daemon and nothing else: the state it lists is what the last verb left in the record.
func (a App) ls(ctx context.Context, args []string) error {
	opts, err := parseLs(args)
	if err != nil {
		return err
	}

	result, err := a.client().ListSandboxes(ctx, opts.all)
	if err != nil {
		return err
	}

	// The daemon answers with both: the sandboxes it read are printed, and the ones it could not are the exit.
	if err := writeTable(a.Out, result.Sandboxes, time.Now()); err != nil {
		return err
	}

	if len(result.Warnings) == 0 {
		return nil
	}

	return errors.New(strings.Join(result.Warnings, "\n"))
}

func writeTable(w io.Writer, sandboxes []models.Sandbox, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tSTATE\tUPTIME\tIP")

	for _, sb := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", sb.ID, orDash(sb.Name), sb.Image, state(sb), uptime(sb, now), address(sb))
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

// state carries the reason a sandbox nobody stopped is stopped, which is the one an operator asks about.
func state(sb models.Sandbox) string {
	if sb.StoppedReason == "" {
		return string(sb.State)
	}

	return fmt.Sprintf("%s (%s)", sb.State, sb.StoppedReason)
}

// uptime is how long the sandbox has been up. A stopped or paused one is not, whatever its record was created.
func uptime(sb models.Sandbox, now time.Time) string {
	if sb.State == models.StateStopped || sb.State == models.StatePaused {
		return "-"
	}

	return now.Sub(sb.CreatedAt).Truncate(time.Second).String()
}

func address(sb models.Sandbox) string {
	if !sb.Address.IsValid() {
		return "-"
	}

	return sb.Address.Addr().String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func parseLs(args []string) (lsOptions, error) {
	var opts lsOptions

	flags := flag.NewFlagSet("shard ls", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.all, "all", false, "include the stopped sandboxes")

	if err := flags.Parse(args); err != nil {
		return lsOptions{}, fmt.Errorf("parse the ls flags: %w", err)
	}

	if rest := flags.Args(); len(rest) != 0 {
		return lsOptions{}, fmt.Errorf("ls takes no argument, got %d", len(rest))
	}

	return opts, nil
}
