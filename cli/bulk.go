package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
)

// sandboxRefs is every sandbox a verb was given; the flag package stopped at the first, so a flag after it is refused until a --.
func sandboxRefs(verb string, rest []string) ([]string, error) {
	if len(rest) == 0 {
		return nil, fmt.Errorf("%s takes one or more sandbox ids or names, got none", verb)
	}
	// A first argument that looks like a flag came after a --, so every one after it is a name too.
	ended := looksLikeFlag(rest[0])
	refs := []string{rest[0]}
	for _, arg := range rest[1:] {
		if arg == "--" && !ended {
			ended = true

			continue
		}
		if looksLikeFlag(arg) && !ended {
			return nil, fmt.Errorf("%s takes one or more sandbox ids or names, got %s", verb, gotArgs(rest))
		}
		refs = append(refs, arg)
	}

	return refs, nil
}

func looksLikeFlag(arg string) bool { return len(arg) > 1 && strings.HasPrefix(arg, "-") }

// each runs one verb on every sandbox given, so one failure leaves the rest to run, as docker does; the failures print last.
func (a App) each(ctx context.Context, refs []string, act func(ref string) error) error {
	var failed []error
	for _, ref := range refs {
		err := act(ref)
		if err == nil {
			continue
		}
		failed = append(failed, err)
		if hopeless(ctx, err) {
			break
		}
	}
	if len(failed) == 0 {
		return nil
	}

	// Run prints the last failure as it prints any error, so only the ones before it print here.
	var unwritten []error
	for _, err := range failed[:len(failed)-1] {
		unwritten = append(unwritten, a.fail(err))
	}

	return errors.Join(append(unwritten, failed[len(failed)-1])...)
}

// hopeless says no later sandbox can fare better: the run was interrupted, nothing answers, or the server refused the caller.
func hopeless(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if _, ok := errors.AsType[*client.ConnectError](err); ok {
		return true
	}
	refused, ok := errors.AsType[*client.APIError](err)

	return ok && (refused.Code == models.CodeUnauthorized || refused.Code == models.CodeForbidden)
}

// fail prints one failure the way main prints the error Run returns, hints and all.
func (a App) fail(err error) error {
	if a.Err == nil {
		return nil
	}
	if _, werr := fmt.Fprintln(a.Err, "shard:", a.forUser(a.located(err))); werr != nil {
		return fmt.Errorf("write the output: %w", werr)
	}

	return nil
}
