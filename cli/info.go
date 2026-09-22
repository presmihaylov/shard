package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/services/daemon"
)

// info prints the substrate a daemon started here runs sandboxes on, and why. It asks the host, not the
// socket, so it answers before a daemon exists and says what the next one picks.
func (a App) info(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("info takes no argument, got %s", strings.Join(args, " "))
	}

	selected := daemon.SelectProvider(a.Provider)
	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintf(w, "provider\t%s\n", selected.Provider)
	fmt.Fprintf(w, "reason\t%s\n", selected.Reason)

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
