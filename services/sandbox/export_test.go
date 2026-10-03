package sandbox

// ExitedExecCap exposes the retention cap to the external test package, so a test pins to the real value.
const ExitedExecCap = exitedExecCap

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
