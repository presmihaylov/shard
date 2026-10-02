package sandbox

// ExitedExecCap exposes the retention cap to the external test package, so a test pins to the real value.
const ExitedExecCap = exitedExecCap

// Locks counts the sandbox locks a verb still holds or waits on, so a test proves none outlives its verbs.
func (s *Service) Locks() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.locks)
}
