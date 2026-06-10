package daemon

import "time"

// clock abstracts time for the scheduler so tests can drive the loop without
// sleeping real wall-time. The production implementation (realClock) delegates
// to the time package; tests inject a fake that fires timers on demand.
//
// The scheduler needs only two operations: read "now" (for the day-budget
// midnight boundary) and wait for a duration with cancellation. waitTimer
// returns a channel that fires after d, plus a stop function the caller must
// invoke to release the timer when it picks a different branch (e.g. ctx done).
type clock interface {
	now() time.Time
	// newTimer returns a channel that receives once after d elapses, and a stop
	// function to release it early. The channel must be drainable in a select
	// alongside ctx.Done().
	newTimer(d time.Duration) (<-chan time.Time, func())
}

// realClock is the production clock backed by the time package.
type realClock struct{}

func (realClock) now() time.Time { return time.Now() }

func (realClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}
