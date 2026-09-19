package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
)

const (
	defaultBackoff = time.Second
	// defaultReset is Docker's healthy-run window: a run this long clears the restart count before the next exit.
	defaultReset = 10 * time.Second
	backoffCap   = models.RestartBackoffCap * time.Second
)

// restartPolicy is what the flags fixed at create: when to start the entrypoint again, and how often.
type restartPolicy struct {
	policy  models.RestartPolicy
	retries int
	backoff time.Duration
	// reset is how long the entrypoint must run since its last start before an exit clears the count.
	reset time.Duration
}

func parseRestart(policy string, retries int, backoff, reset time.Duration) (restartPolicy, error) {
	parsed := restartPolicy{policy: models.RestartPolicy(policy), retries: retries, backoff: backoff, reset: reset}
	switch parsed.policy {
	case models.RestartNo:
		return parsed, nil
	case models.RestartOnFailure, models.RestartAlways:
	default:
		return restartPolicy{}, fmt.Errorf("-restart must be no, on-failure or always, got %q", policy)
	}
	if retries < 0 {
		return restartPolicy{}, fmt.Errorf("-retries cannot be negative, got %d", retries)
	}
	if backoff <= 0 {
		return restartPolicy{}, fmt.Errorf("-backoff must be positive, got %s", backoff)
	}

	return parsed, nil
}

// checkRestartFile is the file mode's half of the policy: the count needs a place to land.
func checkRestartFile(policy restartPolicy, file string) error {
	if policy.policy == models.RestartNo {
		return nil
	}
	if file == "" {
		return errors.New("-restart-file is required with a restart policy")
	}
	if !filepath.IsAbs(file) {
		return fmt.Errorf("-restart-file must be an absolute path, got %q", file)
	}

	return nil
}

// limited says the policy gives up after a fixed number of starts again; always and a zero count never do.
func (r restartPolicy) limited() bool {
	return r.policy == models.RestartOnFailure && r.retries > 0
}

// applies says whether this exit asks for a start again under the policy.
func (r restartPolicy) applies(exit models.ExitStatus) bool {
	switch r.policy {
	case models.RestartAlways:
		return true
	case models.RestartOnFailure:
		return exit.Code != 0 || exit.Signal != 0
	default:
		return false
	}
}

// schedule arms the next start again, or records the give-up once the retries are spent.
func (r restartPolicy) schedule(exit models.ExitStatus, count *models.RestartCount) <-chan time.Time {
	if !r.applies(exit) {
		return nil
	}
	if r.limited() && count.Count >= r.retries {
		count.GaveUp = true

		return nil
	}

	return time.After(r.wait(count.Count))
}

// wait doubles per start again so a crash loop never spins the host, and stops growing at the cap.
func (r restartPolicy) wait(started int) time.Duration {
	wait := r.backoff
	for range started {
		if wait >= backoffCap {
			break
		}
		wait *= 2
	}

	return min(wait, backoffCap)
}
