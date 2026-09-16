package models

import "time"

// ExecState is where one exec is: still running, or ended with an exit status.
type ExecState string

const (
	ExecRunning ExecState = "running"
	ExecExited  ExecState = "exited"
)

// Exec is one command in a sandbox, as the API answers it. It outlives the attach that watches it,
// so a create returns it running and a later get finds how it ended.
type Exec struct {
	ID         string      `json:"exec"`
	Sandbox    string      `json:"sandbox"`
	Command    []string    `json:"command"`
	State      ExecState   `json:"state"`
	ExitStatus *ExitStatus `json:"exit_status"`
	StartedAt  time.Time   `json:"started_at"`
	ExitedAt   *time.Time  `json:"exited_at"`
	// Truncated says the 8 MiB output buffer dropped its oldest bytes, so a replay is not the whole output.
	Truncated bool `json:"truncated"`
}
