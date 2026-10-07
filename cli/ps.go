package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/models"
)

// ps lists the named processes of one sandbox as shard-init last reported them.
func (a App) ps(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("ps", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("ps takes one sandbox id or name, got %s", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	procs, err := c.Processes(ctx, rest[0])
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, procs)
	}

	return writeProcesses(a.Out, procs)
}

func writeProcesses(w io.Writer, procs []models.Process) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	fmt.Fprintln(tw, "NAME\tSTATE\tRESTARTS\tEXIT\tPOLICY\tCOMMAND")

	for _, p := range procs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Status.State, restarts(p), exitOfProcess(p.Status.Exit), p.Restart.Policy, strings.Join(p.Command, " "))
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

// restarts carries the limit of an on-failure policy beside the count, so a reader sees how close it is to giving up.
func restarts(p models.Process) string {
	if p.Restart.Retries > 0 {
		return fmt.Sprintf("%d/%d", p.Status.Restarts, p.Restart.Retries)
	}

	return strconv.Itoa(p.Status.Restarts)
}

func exitOfProcess(exit *models.ExitStatus) string {
	if exit == nil {
		return "-"
	}
	if exit.Signal != 0 {
		return fmt.Sprintf("signal %d", exit.Signal)
	}

	return strconv.Itoa(exit.Code)
}
