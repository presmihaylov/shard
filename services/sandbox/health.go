package sandbox

import (
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
)

// The probe settings a create leaves at zero.
const (
	DefaultHealthInterval = 30
	DefaultHealthTimeout  = 10
	DefaultHealthRetries  = 3
)

// The longest interval and timeout a create takes, so no record goes more than 70 min without a probe result.
const (
	MaxHealthInterval = 3600
	MaxHealthTimeout  = 600
)

// validHealthCheck refuses a probe that names no command, or a setting outside what the probe needs.
func validHealthCheck(hc models.HealthCheck) error {
	if len(hc.Command) == 0 {
		return errors.New("health names no command")
	}
	if hc.Interval < 0 || hc.Timeout < 0 || hc.Retries < 0 {
		return errors.New("health.interval, timeout and retries are counts and cannot be negative")
	}
	if hc.Interval > MaxHealthInterval {
		return fmt.Errorf("health.interval is at most %d seconds, got %d", MaxHealthInterval, hc.Interval)
	}
	if hc.Timeout > MaxHealthTimeout {
		return fmt.Errorf("health.timeout is at most %d seconds, got %d", MaxHealthTimeout, hc.Timeout)
	}

	return nil
}
