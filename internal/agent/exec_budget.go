package agent

import (
	"fmt"
	"sync/atomic"
)

// checkExecBudget atomically increments used and returns an error if the new
// count exceeds maxExec. msgFmt is the tool-specific fmt.Sprintf format string
// with exactly two %d verbs: used count and max (in that order). Returns nil
// when the call is within budget.
//
// Each tool passes its own exact format string so the error text is identical
// to what the original per-tool inline code produced.
//
// Usage (call at the top of each tool's Run method, before any other work):
//
//	const runTestsBudgetMsg = "run_tests budget exhausted (%d/%d calls used); cannot run more test executions for this candidate"
//	if err := checkExecBudget(&t.used, t.maxExec, runTestsBudgetMsg); err != nil {
//	    return "", err
//	}
func checkExecBudget(used *atomic.Int32, maxExec int, msgFmt string) error {
	n := used.Add(1)
	if int(n) > maxExec {
		return fmt.Errorf(msgFmt, int(n)-1, maxExec) //nolint:govet // msgFmt is a caller-controlled constant
	}
	return nil
}
