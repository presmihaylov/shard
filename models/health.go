package models

// HealthCheck is the probe the daemon runs against a running sandbox, fixed at create.
type HealthCheck struct {
	// Command is an argv the provider runs in the sandbox, which passes on exit 0.
	Command []string `json:"command,omitempty"`
	// Interval and Timeout are whole seconds, like the grace of a stop.
	Interval int `json:"interval"`
	Timeout  int `json:"timeout"`
	// Retries is how many probes in a row must fail before the sandbox reads unhealthy.
	Retries int `json:"retries"`
}
