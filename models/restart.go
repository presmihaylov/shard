package models

// RestartPolicy says when shard-init starts a process again inside the sandbox that is still up, and whether the daemon starts it with the sandbox.
type RestartPolicy string

const (
	RestartNo RestartPolicy = "no"
	// RestartOnFailure starts it again after an exit that is not 0, a signal included.
	RestartOnFailure RestartPolicy = "on-failure"
	RestartAlways    RestartPolicy = "always"
	// RestartUnlessStopped is always inside the guest; only the daemon start tells them apart, since a shard stop or kill keeps it down.
	RestartUnlessStopped RestartPolicy = "unless-stopped"
)

// RestartBackoffCap is the longest wait before a start again, however often the backoff doubled.
const RestartBackoffCap = 60

// RestartSpec is the policy fixed at run: Retries caps the starts again, Backoff is the first wait in seconds.
type RestartSpec struct {
	Policy  RestartPolicy `json:"policy" enum:"no,on-failure,always,unless-stopped"`
	Retries int           `json:"retries,omitempty" minimum:"0" doc:"The restarts in a row before the policy gives up; 0 or absent is unlimited. Only on-failure takes it."`
	Backoff int           `json:"backoff" required:"false" minimum:"0" maximum:"60" doc:"The first wait before a restart, in seconds; 0 or absent is 1. It doubles after each restart, up to 60. Only on-failure, always and unless-stopped take it."`
}

// Set reports a policy that starts a process again at all.
func (r RestartSpec) Set() bool { return r.Policy != "" && r.Policy != RestartNo }
