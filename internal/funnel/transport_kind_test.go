package funnel

import (
	"context"
	"fmt"
	"testing"

	llmkit "github.com/dpoage/llmkit"
)

// TestIsTransportError_KindDiscriminates: at StatusCode 0 only Kind ErrServer
// is transport; a pre-wire refusal, another Kind, and a cancelled context
// (a plain error, never an APIError) are not.
func TestIsTransportError_KindDiscriminates(t *testing.T) {
	for name, err := range map[string]error{
		"pre-wire refusal":  &llmkit.APIError{Kind: llmkit.ErrInvalidRequest, StatusCode: 0, Provider: "fake"},
		"overloaded at 0":   &llmkit.APIError{Kind: llmkit.ErrOverloaded, StatusCode: 0, Provider: "fake"},
		"cancelled context": fmt.Errorf("agent: completion failed at iteration 1: %w", context.Canceled),
	} {
		if isTransportError(err) {
			t.Errorf("%s: isTransportError(%v) = true, want false", name, err)
		}
	}
}
