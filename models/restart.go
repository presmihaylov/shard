package models

import "time"

// RestartPolicy says when shard-init starts the entrypoint again inside the sandbox that is still up.
type RestartPolicy string

const (
	RestartNo RestartPolicy = "no"
	// RestartOnFailure starts it again after an exit that is not 0, a signal included.
	RestartOnFailure RestartPolicy = "on-failure"
	RestartAlways    RestartPolicy = "always"
)

// RestartBackoffCap is the longest wait before a start again, however often the backoff doubled.
const RestartBackoffCap = 60

// RestartSpec is the policy fixed at create: Retries caps the starts again, Backoff is the first wait in seconds.
type RestartSpec struct {
	Policy RestartPolicy `json:"policy"`
	// Retries caps the starts again in a row, 0 for unlimited; always never gives up, so it takes none.
	Retries int `json:"retries,omitempty"`
	// Backoff doubles after each start again, up to RestartBackoffCap.
	Backoff int `json:"backoff"`
}

// Set reports a policy that starts the entrypoint again at all.
func (r RestartSpec) Set() bool { return r.Policy != "" && r.Policy != RestartNo }

// RestartCount is what shard-init keeps beside the exit file: the starts again so far.
type RestartCount struct {
	Count int `json:"count"`
	// LastAt is the last start again, zero before the first one.
	LastAt time.Time `json:"last_at,omitzero"`
	// GaveUp says an exit asked for a start again after the retries were spent.
	GaveUp bool `json:"gave_up"`
}

// Restart is the record's view: the policy, and what shard-init did with it on the current run.
type Restart struct {
	RestartSpec
	RestartCount
}
