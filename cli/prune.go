package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
)

// pruneOptions is one parsed shard prune invocation.
type pruneOptions struct {
	force bool
}

// prune removes every stopped sandbox and nothing else, and only once a person said yes or passed --force.
func (a App) prune(ctx context.Context, args []string) error {
	opts, err := parsePrune(args)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}

	result, err := c.ListSandboxes(ctx, true)
	if err != nil {
		return err
	}
	// A record the daemon could not read is not known to be stopped, so it is named and left.
	for _, warning := range result.Warnings {
		a.warn(warning)
	}
	stopped := slices.DeleteFunc(slices.Clone(result.Sandboxes), func(sb client.Sandbox) bool { return sb.State != models.StateStopped })
	if len(stopped) == 0 {
		a.note("no stopped sandboxes to remove")

		return nil
	}

	if !opts.force {
		yes, err := a.askPrune(ctx, stopped)
		if err != nil {
			return err
		}
		if !yes {
			return errors.New("prune cancelled; nothing was removed")
		}
	}

	refs := make([]string, len(stopped))
	for i, sb := range stopped {
		refs[i] = sb.ID
	}

	return a.each(ctx, refs, func(id string) error {
		if err := a.removeByID(ctx, c, id, false); err != nil {
			return err
		}

		return a.print(id)
	})
}

// askPrune shows what would go before it asks, and with no terminal to ask on it refuses rather than delete unasked.
func (a App) askPrune(ctx context.Context, stopped []client.Sandbox) (bool, error) {
	lines := []string{"These stopped sandboxes will be removed:"}
	for _, sb := range stopped {
		lines = append(lines, "  "+sandboxLabel(sb))
	}
	noun := "sandboxes"
	if len(stopped) == 1 {
		noun = "sandbox"
	}
	lines = append(lines, fmt.Sprintf("Remove %d stopped %s?", len(stopped), noun))

	confirm := a.confirm
	if confirm == nil {
		confirm = term.New(a.stdin(), a.Out, os.Getenv).Confirm
	}
	yes, err := confirm(ctx, strings.Join(lines, "\n"), false)
	if errors.Is(err, term.ErrNotTerminal) {
		return false, errors.New("prune asks before it removes and there is no terminal to ask on; pass --force to remove without asking")
	}
	if err != nil {
		return false, err
	}

	return yes, nil
}

// sandboxLabel is the id, and the name beside it when the sandbox has one.
func sandboxLabel(sb client.Sandbox) string {
	if sb.Name == "" {
		return sb.ID
	}

	return sb.ID + " (" + sb.Name + ")"
}

func parsePrune(args []string) (pruneOptions, error) {
	var opts pruneOptions

	flags := newFlags("prune")
	flags.BoolVar(&opts.force, "force", false, "")

	if err := parseVerb(flags, args); err != nil {
		return pruneOptions{}, err
	}
	if rest := flags.Args(); len(rest) != 0 {
		return pruneOptions{}, fmt.Errorf("prune takes no arguments, got %s", gotArgs(rest))
	}

	return opts, nil
}
