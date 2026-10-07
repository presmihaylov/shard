package main

import (
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
)

const (
	defaultBackoff = time.Second
	// defaultReset is Docker's healthy-run window: a run this long clears the restart count before the next exit.
	defaultReset = 10 * time.Second
	backoffCap   = models.RestartBackoffCap * time.Second
)

// restartPolicy is what a run fixed: when to start the process again, and how often.
type restartPolicy struct {
	policy  models.RestartPolicy
	retries int
	backoff time.Duration
	// reset is how long the process must run since its last start before an exit clears the count.
	reset time.Duration
}

func parseRestart(policy string, retries int, backoff, reset time.Duration) (restartPolicy, error) {
	parsed := restartPolicy{policy: models.RestartPolicy(policy), retries: retries, backoff: backoff, reset: reset}
	switch parsed.policy {
	case models.RestartNo:
		return parsed, nil
	case models.RestartOnFailure, models.RestartAlways, models.RestartUnlessStopped:
	default:
		return restartPolicy{}, fmt.Errorf("the restart policy must be no, on-failure, always or unless-stopped, got %q", policy)
	}
	if retries < 0 {
		return restartPolicy{}, fmt.Errorf("the retries cannot be negative, got %d", retries)
	}
	if backoff <= 0 {
		return restartPolicy{}, fmt.Errorf("the backoff must be positive, got %s", backoff)
	}

	return parsed, nil
}

// limited says the policy gives up after a fixed number of starts again; always and a zero count never do.
func (r restartPolicy) limited() bool {
	return r.policy == models.RestartOnFailure && r.retries > 0
}

// applies says whether this exit asks for a start again under the policy.
func (r restartPolicy) applies(exit models.ExitStatus) bool {
	switch r.policy {
	case models.RestartAlways, models.RestartUnlessStopped:
		return true
	case models.RestartOnFailure:
		return exit.Code != 0 || exit.Signal != 0
	default:
		return false
	}
}

// next says what follows an exit after started starts again: one more after the wait, or the state the process ends in.
func (r restartPolicy) next(exit models.ExitStatus, started int) (time.Duration, models.ProcessState) {
	if !r.applies(exit) {
		return 0, models.ProcessExited
	}
	if r.limited() && started >= r.retries {
		return 0, models.ProcessGaveUp
	}

	return r.wait(started), models.ProcessRestarting
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
