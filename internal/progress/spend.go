package progress

// spendAggregator folds KindSpendTick events into run totals. Each tick
// carries CUMULATIVE totals for ONE emitting recorder, identified by the
// event's Role: "" is the funnel's per-run recorder (also the pre-Role wire
// format, so old event streams replay unchanged), RoleReproducer is the repro
// stage's ledger recorder, which lives outside the funnel (engine's
// ledgerRecorder). Keeping the latest value per stream and summing means
// interleaved emitters never clobber each other — during `scan --repro` both
// recorders tick concurrently, and a naive last-write-wins fold would make
// the displayed total flip between two unrelated cumulative counters.
//
// The zero value is ready to use; both consumers (PaneRenderer,
// StatusAccumulator) guard access with their own mutex, so the aggregator
// itself is unsynchronized.
type spendAggregator struct {
	streams map[string]spendTotals
}

// spendTotals is one stream's latest cumulative counters.
type spendTotals struct {
	in, out, cacheRead int64
}

// tick records the latest cumulative totals for the stream named by role.
func (a *spendAggregator) tick(role string, in, out, cacheRead int64) {
	if a.streams == nil {
		a.streams = make(map[string]spendTotals, 2)
	}
	a.streams[role] = spendTotals{in: in, out: out, cacheRead: cacheRead}
}

// totals sums the latest cumulative totals across all streams.
func (a *spendAggregator) totals() (in, out, cacheRead int64) {
	for _, t := range a.streams {
		in += t.in
		out += t.out
		cacheRead += t.cacheRead
	}
	return in, out, cacheRead
}
