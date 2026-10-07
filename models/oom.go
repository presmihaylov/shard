package models

import "time"

// OOM is the record of the host's memory kills of a sandbox, which the daemon starts again after each one.
type OOM struct {
	// Kills counts every kill the sandbox has had.
	Kills int `json:"kills"`
	// InARow counts the kills since a run last outlasted the healthy run; the backoff grows with it.
	InARow int `json:"in_a_row"`
	// KilledAt is the last kill, as the daemon saw it.
	KilledAt time.Time `json:"killed_at"`
	// RestartAt is when the daemon starts the sandbox again, zero once a start ran or a stop called it off.
	RestartAt time.Time `json:"restart_at,omitzero"`
}

// RestartDue reports a start again the daemon still owes the sandbox.
func (o *OOM) RestartDue() bool { return o != nil && !o.RestartAt.IsZero() }
