package agent

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/vishvananda/netlink"

	"github.com/walzen-group/kuport/internal/datapath"
	"github.com/walzen-group/kuport/internal/metrics"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

// Snapshot is what the last successful apply programmed, the zero Snapshot
// before the first one. It makes the Reconciler a metrics.Source.
func (r *Reconciler) Snapshot() metrics.Snapshot {
	if s := r.snap.Load(); s != nil {
		return *s
	}
	return metrics.Snapshot{}
}

// Counters returns every series' packets and bytes across applies.
func (r *Reconciler) Counters() (map[datapath.LiveKey]datapath.CounterValue, error) {
	if r.Totals == nil {
		return nil, errors.New("no counters configured")
	}
	return r.Totals.Read()
}

// Flows dumps the node's conntrack table, giving up after five seconds.
func (r *Reconciler) Flows(ctx context.Context) ([]*netlink.ConntrackFlow, error) {
	if r.Conntrack == nil {
		return nil, errors.New("no conntrack reader configured")
	}
	return datapath.DumpFlows(ctx, r.Conntrack)
}

// recordApply stores what a successful apply programmed for the collector.
func (r *Reconciler) recordApply(in kreconcile.Inputs, res kreconcile.Result) {
	r.snap.Store(&metrics.Snapshot{
		Index: res.Metrics,
		DNAT:  res.State.DNAT,
		Links: linkInfos(in, res.State, r.NodeName),
	})
}

// linkInfos lists the return-link devices the State holds with the Link they
// come from: Link slot-<n> is device kup-<n>, and its other peer is the far
// end.
func linkInfos(in kreconcile.Inputs, st datapath.State, node string) []metrics.LinkInfo {
	held := map[string]bool{}
	for _, l := range st.Links {
		held[l.Name] = true
	}
	var out []metrics.LinkInfo
	for _, l := range in.Links {
		slot, ok := strings.CutPrefix(l.Name, "slot-")
		if !ok || !held["kup-"+slot] {
			continue
		}
		var peer string
		mine := false
		for _, p := range l.Spec.Peers {
			if p == node {
				mine = true
			} else {
				peer = p
			}
		}
		if !mine {
			continue
		}
		out = append(out, metrics.LinkInfo{Device: "kup-" + slot, Slot: slot, Class: l.Spec.ClassName, Peer: peer})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}
