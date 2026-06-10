package progress

import "time"

// timeRound is the granularity durations are rounded to for display, keeping
// human-readable lines free of nanosecond noise.
const timeRound = 100 * time.Millisecond

// timeClock is the wall-clock layout used for schedule ETAs in log output.
const timeClock = "15:04:05"

// shortSHA abbreviates a commit SHA for display, leaving short/empty values
// untouched. Mirrors the helpers in the cli/daemon packages so progress output
// matches the rest of Bugbot's formatting.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
