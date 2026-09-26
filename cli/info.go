package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/services/daemon"
)

// info prints the substrate a daemon started now over this root would run sandboxes on, and why. It
// asks the host and not the socket, so it answers before a daemon exists; what the one already up
// runs is shard daemon status, which can differ when that daemon was started with other flags.
func (a App) info(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("info takes no argument, got %s", strings.Join(args, " "))
	}

	selected, err := daemon.SelectProvider(a.Provider, a.Root)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintf(w, "provider\t%s\n", selected.Provider)
	fmt.Fprintf(w, "reason\t%s\n", selected.Reason)

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
