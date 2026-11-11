package usecase

import "time"

// SetClock replaces the clock used by r, for tests.
func (r *Retention) SetClock(now func() time.Time) { r.now = now }
