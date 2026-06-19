package progress

import "time"

// timeRound is the granularity durations are rounded to for display, keeping
// human-readable lines free of nanosecond noise.
const timeRound = 100 * time.Millisecond

// timeClock is the wall-clock layout used for schedule ETAs in log output.
const timeClock = "15:04:05"

