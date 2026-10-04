package sandbox

import (
	"errors"

	"github.com/presmihaylov/shard/models"
)

// FailedGeneric is what a public route says of a failed sandbox whose cause no audited text covers.
const FailedGeneric = "the sandbox failed; the daemon log has the cause"

// publicError is an error whose text names only what the caller sent, never a host path, a pid or the root.
type publicError interface {
	Public() string
}

// PublicText is the text of the first error in err's tree that opts in, so a wrapper's host context drops out.
func PublicText(err error) (string, bool) {
	var public publicError
	if !errors.As(err, &public) {
		return "", false
	}

	return public.Public(), true
}

// PublicReason is the failed reason a public route answers; a record older than FailedPublic gets the generic text.
func PublicReason(sb models.Sandbox) string {
	if sb.FailedPublic != "" || sb.FailedReason == "" {
		return sb.FailedPublic
	}

	return FailedGeneric
}

// CauseError keeps its own words public and lets only the daemon log see a cause that does not opt in.
type CauseError struct {
	Text string
	Err  error
}

func (e *CauseError) Error() string { return e.Text + ": " + e.Err.Error() }

func (e *CauseError) Unwrap() error { return e.Err }

func (e *CauseError) Public() string {
	if public, ok := PublicText(e.Err); ok {
		return e.Text + ": " + public
	}

	return e.Text + "; the daemon log has the cause"
}
