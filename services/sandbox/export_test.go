package sandbox

import "context"

// ExitedExecCap exposes the retention cap to the external test package, so a test pins to the real value.
const ExitedExecCap = exitedExecCap

// Locks counts the sandbox locks a verb still holds or waits on, so a test proves none outlives its verbs.
func (s *Service) Locks() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.locks)
}

// Hold takes the lock of one sandbox as a verb does, so a test lines other verbs up behind it.
func (s *Service) Hold(ctx context.Context, id string) (func(), error) {
	return s.lock(ctx, id)
}

// Waiters counts the verbs that hold or wait on the lock of one sandbox.
func (s *Service) Waiters(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.locks[id]
	if !ok {
		return 0
	}

	return l.refs
}

// ExecsHeld counts the execs the service still holds for one sandbox, which no route reaches once its record is gone.
func (s *Service) ExecsHeld(id string) int {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	held := 0
	for _, session := range s.execs {
		if session.sandboxID == id {
			held++
		}
	}

	return held
}

// The running-exec bounds, so a test pins to the real values.
const (
	MaxRunningExecsPerSandbox = maxRunningExecsPerSandbox
	MaxRunningExecs           = maxRunningExecs
)

// AdmitExec takes a running slot as a create does, so a test fills the daemon bound with sandboxes the fake repository does not hold.
func (s *Service) AdmitExec(id string) error { return s.admitExec(id) }

// ReleaseExec frees a slot that AdmitExec took.
func (s *Service) ReleaseExec(id string) { s.releaseExec(id) }

// RunningExecs counts the running slots one sandbox holds, and the daemon holds in all.
func (s *Service) RunningExecs(id string) (int, int) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	return s.running[id], s.runningAll
}
