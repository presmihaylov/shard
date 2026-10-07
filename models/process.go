package models

import (
	"regexp"
	"time"
)

// MaxProcesses bounds the named processes of one sandbox, so the whole table fits the sealed status channel.
const MaxProcesses = 16

// MaxProcessName is the longest process name; it names a log file on the host.
const MaxProcessName = 32

var processName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// ValidProcessName reports a name that is safe as a file name on the host and a column of ps.
func ValidProcessName(name string) bool { return processName.MatchString(name) }

// ProcessState is where one named process stands.
type ProcessState string

const (
	ProcessRunning ProcessState = "running"
	// ProcessRestarting is an exit the policy starts again after its backoff.
	ProcessRestarting ProcessState = "restarting"
	// ProcessExited is an exit the policy does not start again.
	ProcessExited ProcessState = "exited"
	// ProcessKilled is a process shard kill ended.
	ProcessKilled ProcessState = "killed"
	// ProcessGaveUp is an on-failure process that spent its retries.
	ProcessGaveUp ProcessState = "gave-up"
	// ProcessStopped is a process the sandbox stopped under; only the host says it, never shard-init.
	ProcessStopped ProcessState = "stopped"
)

// Ended reports a state no start again follows on this run of the sandbox.
func (s ProcessState) Ended() bool {
	return s == ProcessExited || s == ProcessKilled || s == ProcessGaveUp || s == ProcessStopped
}

// Process is one named process shard run started in a sandbox: what to run, and the last status the daemon read.
type Process struct {
	Name    string      `json:"name"`
	Command []string    `json:"command"`
	Env     []string    `json:"env,omitempty"`
	WorkDir string      `json:"workdir,omitempty"`
	User    string      `json:"user,omitempty"`
	Restart RestartSpec `json:"restart"`
	// Killed is set by shard kill and keeps an unless-stopped process down; a run of the same name clears it.
	Killed bool          `json:"killed,omitempty"`
	Status ProcessStatus `json:"status"`
}

// ProcessStatus is what shard-init last said of one process.
type ProcessStatus struct {
	State ProcessState `json:"state" enum:"running,restarting,exited,killed,gave-up,stopped"`
	// Restarts counts the starts again since the process was run or its sandbox started.
	Restarts int `json:"restarts"`
	// Exit is the last exit, nil before the first one.
	Exit      *ExitStatus `json:"exit,omitempty"`
	StartedAt time.Time   `json:"started_at,omitzero"`
}

// ProcessReport is shard-init's word on one process.
type ProcessReport struct {
	Name string `json:"name"`
	ProcessStatus
	// Seq orders the reports of one boot, so a reader that sees two of a name keeps the later.
	Seq uint64 `json:"seq"`
}

// ProcessSpec is one process for shard-init to start; an empty Env, WorkDir or User takes the sandbox's own, as an exec does.
type ProcessSpec struct {
	Name    string
	Argv    []string
	Env     []string
	WorkDir string
	User    string
	Restart RestartSpec
}
