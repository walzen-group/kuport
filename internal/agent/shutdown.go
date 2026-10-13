package agent

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/walzen-group/kuport/internal/datapath"
)

// HostDatapath is the production Datapath: it renders a State into a Plan and
// applies it, and tears down through the same host handles. It is the only path
// that needs NET_ADMIN; tests substitute a fake Datapath.
type HostDatapath struct {
	Handles datapath.Handles
	// Counters, when set, folds the live counters into its totals before each
	// Apply replaces the table.
	Counters *datapath.Counters
}

// Apply renders the desired State and writes it to the host.
func (d HostDatapath) Apply(ctx context.Context, state datapath.State) error {
	apply := func() error { return datapath.Apply(ctx, datapath.Render(state), d.Handles) }
	if d.Counters == nil {
		return apply()
	}
	return d.Counters.FoldAndApply(apply)
}

// Teardown removes everything kuport owns on this node.
func (d HostDatapath) Teardown(ctx context.Context) error {
	return datapath.Teardown(ctx, d.Handles)
}

// TeardownRunnable is a manager.Runnable that tears the datapath down on
// shutdown. It blocks until its context is cancelled, which the signal handler
// does on SIGTERM or SIGINT, then runs Teardown so a node leaving a class stops
// holding state nobody wants. A crashed agent skips this and the next Apply
// reconciles its leftovers away, which is why there is no startup cleanup to
// fight that.
//
// Keep, when it returns a State holding egress rules, replaces the teardown
// with an Apply of that State: the node's fail-closed Egress pods stay held in
// the blackhole until the next agent's first pass, so a rolling update leaves
// no window where their default-route traffic leaves from this node.
type TeardownRunnable struct {
	DP   Datapath
	Keep func() datapath.State
	Log  logr.Logger
}

// Start blocks until ctx is done, then tears down. It returns Teardown's error
// so a failed cleanup surfaces in the manager's shutdown.
func (t TeardownRunnable) Start(ctx context.Context) error {
	<-ctx.Done()
	// The parent context is already cancelled, so give the datapath a live one.
	if t.Keep != nil {
		if keep := t.Keep(); len(keep.Egress) > 0 {
			t.Log.Info("shutting down; keeping fail-closed egress pods blocked", "pods", len(keep.Egress))
			return t.DP.Apply(context.Background(), keep)
		}
	}
	t.Log.Info("shutting down; tearing down datapath")
	return t.DP.Teardown(context.Background())
}
