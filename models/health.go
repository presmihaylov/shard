package models

import "time"

// HealthCheck is the probe the daemon runs against a running sandbox, fixed at create.
type HealthCheck struct {
	// Command is an argv the provider runs in the sandbox, which passes on exit 0. HTTP is the other kind.
	Command []string   `json:"command,omitempty"`
	HTTP    *HTTPProbe `json:"http,omitempty"`
	// Interval and Timeout are whole seconds, like the grace of a stop.
	Interval int `json:"interval"`
	Timeout  int `json:"timeout"`
	// Retries is how many probes in a row must fail before the sandbox reads unhealthy.
	Retries int `json:"retries"`
}

// HTTPProbe is a GET from the host to the sandbox's address, which passes on a 2xx or 3xx answer.
type HTTPProbe struct {
	Port int    `json:"port"`
	Path string `json:"path"`
}

// HealthStatus is what the probes found so far.
type HealthStatus string

const (
	// HealthStarting is a run no probe has passed on yet.
	HealthStarting HealthStatus = "starting"
	HealthHealthy  HealthStatus = "healthy"
	// HealthUnhealthy is a run whose last probes failed as many times in a row as the retries allow.
	HealthUnhealthy HealthStatus = "unhealthy"
)

// Health is the result of the probes on the current run, absent on a sandbox that has no probe.
type Health struct {
	Status HealthStatus `json:"status"`
	// CheckedAt is the last probe, zero before the first one of the run.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// Failures counts the probes that failed in a row; one that passes sets it back to zero.
	Failures int `json:"failures"`
}
