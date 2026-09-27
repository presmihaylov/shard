package runsc

import "time"

// SetSettle shortens the grace a cancelled create or restore gets, so a test of its expiry does not wait out the real one.
func (r *Runner) SetSettle(d time.Duration) { r.settle = d }
