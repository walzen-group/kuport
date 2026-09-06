package agent

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// TestTeardownRunnableOnShutdown asserts the runnable tears the datapath down
// when its context is cancelled — which is exactly what the SIGTERM/SIGINT
// signal handler does. An agent that left its rules behind on a clean shutdown
// would be a real bug, so this pins that Teardown runs.
func TestTeardownRunnableOnShutdown(t *testing.T) {
	dp := &fakeDatapath{}
	run := TeardownRunnable{DP: dp, Log: logr.Discard()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run.Start(ctx) }()

	// Simulate SIGTERM: the signal handler cancels the context.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}

	if dp.teardowns != 1 {
		t.Errorf("Teardown called %d times, want 1", dp.teardowns)
	}
}

// TestTeardownRunnableBlocksUntilCancel confirms the runnable does not tear down
// while the context is live.
func TestTeardownRunnableBlocksUntilCancel(t *testing.T) {
	dp := &fakeDatapath{}
	run := TeardownRunnable{DP: dp, Log: logr.Discard()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run.Start(ctx) }()

	select {
	case <-done:
		t.Fatal("Start returned before the context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
	if dp.teardowns != 0 {
		t.Errorf("Teardown ran early: %d times", dp.teardowns)
	}
}
