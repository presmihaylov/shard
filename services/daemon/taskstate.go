package daemon

import (
	"sync"

	"github.com/presmihaylov/shard/services/api"
)

// The state GET /v0/daemon reports for a supervised task; shard daemon status exits non-zero on TaskBackoff.
const (
	TaskRunning = "running"
	TaskBackoff = "backoff"
	TaskDone    = "done"
)

// taskStates is the live state of every supervised task, shared by supervise and the GET /v0/daemon handler.
type taskStates struct {
	mu     sync.Mutex
	order  []string
	status map[string]*taskStatus
}

type taskStatus struct {
	state     string
	restarts  int
	lastError string
}

// newTaskStates seeds one record per task in registration order, each running before its first supervise pass.
func newTaskStates(tasks []Task) *taskStates {
	ts := &taskStates{status: make(map[string]*taskStatus, len(tasks))}
	for _, t := range tasks {
		ts.at(t.Name()).state = TaskRunning
	}

	return ts
}

// at returns the record for name, creating it in order the first time, so supervise never panics on an unseeded task.
func (ts *taskStates) at(name string) *taskStatus {
	s := ts.status[name]
	if s == nil {
		s = &taskStatus{}
		ts.status[name] = s
		ts.order = append(ts.order, name)
	}

	return s
}

func (ts *taskStates) running(name string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.at(name).state = TaskRunning
}

// backoff records a failed run: the task waits to restart, the count rises, and the error is kept for the status.
func (ts *taskStates) backoff(name string, err error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	s := ts.at(name)
	s.state = TaskBackoff
	s.restarts++
	s.lastError = err.Error()
}

func (ts *taskStates) done(name string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.at(name).state = TaskDone
}

func (ts *taskStates) snapshot() []api.TaskState {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]api.TaskState, 0, len(ts.order))
	for _, name := range ts.order {
		s := ts.status[name]
		out = append(out, api.TaskState{Name: name, State: s.state, Restarts: s.restarts, LastError: s.lastError})
	}

	return out
}
