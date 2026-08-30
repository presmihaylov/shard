package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/models"
)

// lsOptions is one parsed shard ls invocation.
type lsOptions struct {
	all bool
}

// ls reads the records and nothing else: the state a record holds is what the last verb left there.
func (a App) ls(_ context.Context, args []string) error {
	opts, err := parseLs(args)
	if err != nil {
		return err
	}

	repo, err := a.deps().repo()
	if err != nil {
		return err
	}

	// List answers with both: the sandboxes it read are printed, and the ones it could not are the exit.
	sandboxes, unreadable := repo.List()

	// A stopped sandbox holds no process, so it is shown on --all only.
	if !opts.all {
		sandboxes = slices.DeleteFunc(sandboxes, func(sb models.Sandbox) bool { return sb.State == models.StateStopped })
	}

	if err := writeTable(a.Out, sandboxes, time.Now()); err != nil {
		return err
	}

	return unreadable
}

func writeTable(w io.Writer, sandboxes []models.Sandbox, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	fmt.Fprintln(tw, "ID\tNAME\tIMAGE\tSTATE\tUPTIME\tIP")

	for _, sb := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", sb.ID, orDash(sb.Name), sb.Image, sb.State, uptime(sb, now), address(sb))
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

// uptime is how long the sandbox has been up. A stopped one is not, whatever its record was created.
func uptime(sb models.Sandbox, now time.Time) string {
	if sb.State == models.StateStopped {
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
