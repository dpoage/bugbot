package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMockQueueThenDefault(t *testing.T) {
	m := NewMock(MockResponse{Result: Result{ExitCode: 7}})
	m.EnqueueResponse(MockResponse{Result: Result{ExitCode: 0, Stdout: "first"}})
	m.EnqueueResponse(MockResponse{Err: errors.New("boom")})

	ctx := context.Background()

	r1, err := m.Exec(ctx, Spec{Cmd: []string{"a"}})
	if err != nil || r1.Stdout != "first" {
		t.Fatalf("call 1: r=%+v err=%v", r1, err)
	}

	_, err = m.Exec(ctx, Spec{Cmd: []string{"b"}})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("call 2 expected boom error, got %v", err)
	}

	// Queue exhausted -> default.
	r3, err := m.Exec(ctx, Spec{Cmd: []string{"c"}})
	if err != nil || r3.ExitCode != 7 {
		t.Fatalf("call 3 expected default exit 7, got r=%+v err=%v", r3, err)
	}

	if got := m.CallCount(); got != 3 {
		t.Fatalf("CallCount = %d, want 3", got)
	}
	calls := m.Calls()
	if len(calls) != 3 || calls[0].Spec.Cmd[0] != "a" || calls[2].Spec.Cmd[0] != "c" {
		t.Fatalf("recorded calls wrong: %+v", calls)
	}
}

func TestMockResponseFuncTakesPrecedence(t *testing.T) {
	m := NewMock(MockResponse{Result: Result{ExitCode: 99}})
	m.EnqueueResponse(MockResponse{Result: Result{ExitCode: 1}})
	m.ResponseFunc = func(n int, spec Spec) (Result, error) {
		return Result{ExitCode: n, Stdout: spec.Cmd[0]}, nil
	}

	for i := 0; i < 3; i++ {
		r, err := m.Exec(context.Background(), Spec{Cmd: []string{"cmd"}})
		if err != nil || r.ExitCode != i {
			t.Fatalf("call %d: r=%+v err=%v", i, r, err)
		}
	}
}

func TestMockConcurrentSafe(t *testing.T) {
	m := NewMock(MockResponse{Result: Result{ExitCode: 0}})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.Exec(context.Background(), Spec{Cmd: []string{"x"}})
		}()
	}
	wg.Wait()
	if got := m.CallCount(); got != 50 {
		t.Fatalf("CallCount = %d, want 50", got)
	}
}

func TestMockReset(t *testing.T) {
	m := NewMock(MockResponse{})
	m.EnqueueResponse(MockResponse{Result: Result{ExitCode: 5}})
	_, _ = m.Exec(context.Background(), Spec{Cmd: []string{"x"}})
	m.Reset()
	if m.CallCount() != 0 {
		t.Fatal("Reset should clear calls")
	}
	// Queue cleared -> default returned.
	r, _ := m.Exec(context.Background(), Spec{Cmd: []string{"y"}})
	if r.ExitCode != 0 {
		t.Fatalf("after reset expected default exit 0, got %d", r.ExitCode)
	}
}
