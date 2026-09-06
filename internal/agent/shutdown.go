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
}

// Apply renders the desired State and writes it to the host.
func (d HostDatapath) Apply(ctx context.Context, state datapath.State) error {
	return datapath.Apply(ctx, datapath.Render(state), d.Handles)
}

// Teardown removes everything kuport owns on this node.
func (d HostDatapath) Teardown(ctx context.Context) error {
	return datapath.Teardown(ctx, d.Handles)
}

// TeardownRunnable is a manager.Runnable that tears the datapath down on
// shutdown. It blocks until its context is cancelled — which the signal handler
// does on SIGTERM or SIGINT — then runs Teardown so a node leaving a class stops
// holding state nobody wants. A crashed agent skips this and the next Apply
// reconciles its leftovers away, which is why there is no startup cleanup to
// fight that.
type TeardownRunnable struct {
	DP  Datapath
	Log logr.Logger
}

// Start blocks until ctx is done, then tears down. It returns Teardown's error
// so a failed cleanup surfaces in the manager's shutdown.
func (t TeardownRunnable) Start(ctx context.Context) error {
	<-ctx.Done()
	t.Log.Info("shutting down; tearing down datapath")
	// The parent context is already cancelled, so give Teardown a live one.
	return t.DP.Teardown(context.Background())
}
