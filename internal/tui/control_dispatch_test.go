package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/dpoage/bugbot/internal/engine"
)

// TestControlDispatch_ReviewPRNotSupported verifies review is deliberately
// NOT proxied over the control socket (bugbot-2p8z.4's verb table excludes
// it — see control_dispatch.go's doc comment): controlDispatch.ReviewPR
// must fail with a clear, stable error rather than silently no-op'ing or
// panicking on a nil client.
func TestControlDispatch_ReviewPRNotSupported(t *testing.T) {
	c := newControlDispatch(nil)
	res, err := c.ReviewPR(context.Background(), engine.ReviewPROpts{PRNumber: 42})
	if res != nil {
		t.Errorf("ReviewPR() result = %+v, want nil", res)
	}
	if !errors.Is(err, errReviewPRNotSupportedOverAttach) {
		t.Errorf("ReviewPR() error = %v, want errReviewPRNotSupportedOverAttach", err)
	}
}
