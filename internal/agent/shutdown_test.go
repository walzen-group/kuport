package agent

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/walzen-group/kuport/internal/datapath"
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

// TestTeardownRunnableKeepsFailClosedPodsBlocked: with fail-closed Egress pods
// on the node, shutdown applies the keep state, so the pods stay held in the
// blackhole until the next agent's first pass, and nothing is torn down.
func TestTeardownRunnableKeepsFailClosedPodsBlocked(t *testing.T) {
	keep := datapath.State{Egress: []datapath.EgressRule{{SrcAddr: netip.MustParseAddr("10.244.5.5"), Mark: datapath.EgressBlockedMark}}}
	for _, c := range []struct {
		name      string
		keep      datapath.State
		applies   int
		teardowns int
	}{
		{"fail-closed pods", keep, 1, 0},
		{"nothing to keep", datapath.State{}, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			dp := &fakeDatapath{}
			run := TeardownRunnable{DP: dp, Keep: func() datapath.State { return c.keep }, Log: logr.Discard()}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := run.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if len(dp.applied) != c.applies || dp.teardowns != c.teardowns {
				t.Fatalf("applies %d, teardowns %d; want %d and %d", len(dp.applied), dp.teardowns, c.applies, c.teardowns)
			}
			if c.applies == 1 && !reflect.DeepEqual(dp.applied[0], keep) {
				t.Errorf("applied %+v, want the keep state", dp.applied[0])
			}
		})
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
