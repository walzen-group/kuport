// Package agent is the shell around kuport's pure reconcile: it watches the
// cluster, feeds a whole-node snapshot into reconcile.Compute, applies the
// datapath state that comes back, and writes the status this node is
// responsible for. It also performs the host checks Compute cannot make,
// because they need root: the CNI's tunnel mode, a return-link port collision,
// and the underlay MTU.
//
// The reconcile is level-driven and whole-node. Every watch event maps to one
// fixed request key, so any change recomputes the complete desired state for
// this node. A mapping that was deleted disappears by being absent from the
// next State, never by anything here remembering to remove it. Every agent in
// the DaemonSet runs this same loop over the same objects and, because the
// computation is deterministic, they agree without a controller, a leader
// election, or a Lease between them.
package agent

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

// reconcileKey is the single request key every watch event maps to. Because the
// reconcile is whole-node, the key names the node, not an object; the controller
// runtime's rate limiter coalesces a burst of events on this one key into a
// single recompute.
var reconcileKey = types.NamespacedName{Name: "kuport-node"}

// Datapath is the host side the reconciler drives: render-and-apply the desired
// State, and tear everything down on shutdown. The real implementation renders
// through datapath.Render and writes through datapath.Apply; tests supply a fake
// so the reconcile runs with no root and no kernel.
type Datapath interface {
	Apply(ctx context.Context, state datapath.State) error
	Teardown(ctx context.Context) error
}

// Host answers the questions the pure reconcile cannot, because they need the
// node's kernel and the CNI's configuration.
type Host interface {
	// TunnelMode reports whether the CNI runs in tunnel mode. known is false
	// when the mode cannot be determined (Cilium's config is absent, or carries
	// no recognised mode key); the caller must not refuse a class on a mode it
	// could not read.
	TunnelMode(ctx context.Context) (ok bool, known bool, err error)
	// CNITunnelPort is the UDP port the CNI's own tunnel uses, 8472 for Cilium.
	CNITunnelPort() int
	// UnderlayMTU is the MTU of the interface bearing the node's InternalIP.
	UnderlayMTU(internalIP string) (int, error)
	// InterfaceAddr is the first IPv4 address on a named local interface.
	InterfaceAddr(name string) (string, error)
}

// Reconciler is the whole-node reconcile loop. Everything host-facing is behind
// the Datapath and Host interfaces so the loop is testable against fakes.
type Reconciler struct {
	Client   client.Client
	NodeName string
	DP       Datapath
	Host     Host

	// Now injects the timestamp Compute stamps on conditions; defaults to
	// metav1.Now. Held here so tests pin it.
	Now func() metav1.Time

	Log logr.Logger

	// ready flips true after the first reconcile completes; the readiness probe
	// reads it.
	ready atomic.Bool

	// lastState fingerprints the last applied datapath state, so an applied
	// change logs at info and a no-op pass at debug.
	lastState string
}

// Ready reports whether at least one reconcile has completed. The caches are
// synced before the manager starts this controller, so this flag is the second
// half of "caches synced and at least one reconcile completed".
func (r *Reconciler) Ready() bool { return r.ready.Load() }

// now returns the injected clock or metav1.Now.
func (r *Reconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}
	return metav1.Now()
}

// SetupWithManager wires the five watches to the single whole-node request key.
// Node stays cluster-wide: host checks read only this node from the cache, but
// class selection needs every node's labels and InternalIP, so the informer is
// not filtered.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	toNode := handler.EnqueueRequestsFromMapFunc(
		func(context.Context, client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: reconcileKey}}
		})

	return ctrl.NewControllerManagedBy(mgr).
		Named("kuport-agent").
		Watches(&v1alpha1.PortMapClass{}, toNode).
		Watches(&v1alpha1.PortMap{}, toNode).
		Watches(&discoveryv1.EndpointSlice{}, toNode).
		Watches(&corev1.Node{}, toNode).
		Watches(&corev1.Namespace{}, toNode).
		Complete(r)
}

// Reconcile recomputes the complete desired state for this node and imposes it:
// assemble inputs from the cache, run the host checks, compute, apply the
// datapath, then write the status this node owns.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	in, err := r.buildInputs(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("assemble inputs: %w", err)
	}

	res := kreconcile.Compute(in)

	if err := r.applyState(ctx, res.State); err != nil {
		return reconcile.Result{}, fmt.Errorf("apply datapath: %w", err)
	}

	hs := r.hostState(ctx, in)

	if err := r.writeStatuses(ctx, res, hs); err != nil {
		return reconcile.Result{}, fmt.Errorf("write status: %w", err)
	}

	r.ready.Store(true)
	return reconcile.Result{}, nil
}

// applyState renders and applies the desired State, logging an applied change at
// info and a no-op pass at debug. The datapath's failure mode is a silent drop,
// so a change always leaves a line saying what was written.
func (r *Reconciler) applyState(ctx context.Context, st datapath.State) error {
	if err := r.DP.Apply(ctx, st); err != nil {
		return err
	}
	fp := fmt.Sprintf("%d dnat, %d exempt, %d mark, %d links, %d rules, %d routes",
		len(st.DNAT), len(st.Exempt), len(st.Mark), len(st.Links), len(st.Rules), len(st.Routes))
	if fp != r.lastState {
		r.Log.Info("applied datapath state", "state", fp)
		r.lastState = fp
	} else {
		r.Log.V(1).Info("datapath unchanged", "state", fp)
	}
	return nil
}

// hostState gathers, once per pass, the host facts the status writer overlays
// onto the class conditions and node rows. A failed read is logged and left
// zero rather than failing the whole reconcile: a mapping's rules are already
// applied, and a missing MTU or unreadable CNI config should not stop status.
func (r *Reconciler) hostState(ctx context.Context, in kreconcile.Inputs) hostState {
	hs := hostState{cniPort: r.Host.CNITunnelPort()}

	ok, known, err := r.Host.TunnelMode(ctx)
	switch {
	case err != nil:
		r.Log.Error(err, "read CNI tunnel mode")
	case !known:
		r.Log.Info("CNI tunnel mode unknown; not refusing classes on tunnel grounds")
	default:
		hs.tunnelOK, hs.tunnelKnown = ok, true
	}

	if ip := internalIPOf(in.Nodes, r.NodeName); ip != "" {
		if mtu, err := r.Host.UnderlayMTU(ip); err != nil {
			r.Log.Error(err, "read underlay MTU", "internalIP", ip)
		} else {
			hs.underlayMTU = mtu
			hs.linkMTU = datapath.LinkMTU(mtu)
		}
	}
	return hs
}

// hostState is the per-pass bundle of host facts the status writer needs.
type hostState struct {
	tunnelOK    bool
	tunnelKnown bool
	cniPort     int
	underlayMTU int
	linkMTU     int
}

// internalIPOf returns the InternalIP of a named node from the input set.
func internalIPOf(nodes []*corev1.Node, name string) string {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				return a.Address
			}
		}
	}
	return ""
}
