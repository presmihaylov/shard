package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/services/daemon"
)

// info prints the provider a daemon started now over this root with no --provider would pick, and why; it asks the host, not the socket.
func (a App) info(_ context.Context, args []string) error {
	rest, format, err := parseFormatArgs("info", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("info takes no arguments, got %s", gotArgs(rest))
	}
	if err := a.hostOnly("info"); err != nil {
		return err
	}
	selected, err := daemon.SelectProvider("", a.Root)
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, infoView{Provider: selected.Provider, Reason: selected.Reason, Unreadable: selected.Unreadable})
	}

	fields := section{columns: []string{"FIELD", "VALUE"}, rows: [][]string{
		{"provider", selected.Provider},
		{"reason", selected.Reason},
	}}
	if selected.Unreadable != "" {
		fields.rows = append(fields.rows, []string{"unreadable", selected.Unreadable})
	}

	return writeSections(a.Out, fields)
}
