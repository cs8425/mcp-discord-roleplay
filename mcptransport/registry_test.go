package mcptransport

import (
	"context"
	"sync"
	"testing"
)

// TestDispatchRecoversPanic guards the process-crash fix: a panicking tool
// handler must be recovered by Dispatch and surfaced as an isError result, not
// allowed to propagate (which under stdio would kill the whole server).
func TestDispatchRecoversPanic(t *testing.T) {
	reg := NewRegistry("")
	if err := reg.Register(&Tool{Name: "boom"}, func(context.Context, *Call) (*ToolResult, error) {
		panic("kaboom")
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	res, err := reg.Dispatch(context.Background(), &Call{Name: "boom"})
	if err != nil {
		t.Fatalf("Dispatch err = %v, want nil (panic must be recovered into an error result)", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("res = %+v, want a non-nil isError result", res)
	}
}

func TestDispatchUnknownTool(t *testing.T) {
	reg := NewRegistry("")
	if _, err := reg.Dispatch(context.Background(), &Call{Name: "no-such-tool"}); err == nil {
		t.Fatalf("want an error for an unknown tool")
	}
}

// TestDispatchConcurrentNoRace runs many concurrent tools/call dispatches
// through a single Registry to keep `go test -race` clean on the serve-time
// (read-only) handler map.
func TestDispatchConcurrentNoRace(t *testing.T) {
	reg := NewRegistry("")
	var mu sync.Mutex
	var n int
	if err := reg.Register(&Tool{Name: "ping"}, func(context.Context, *Call) (*ToolResult, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return ResultText("ok"), nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	const goroutines, iterations = 64, 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				res, err := reg.Dispatch(context.Background(), &Call{Name: "ping"})
				if err != nil || res == nil || res.IsError {
					t.Errorf("dispatch failed: err=%v res=%+v", err, res)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n != goroutines*iterations {
		t.Fatalf("handler ran %d times, want %d", n, goroutines*iterations)
	}
}
