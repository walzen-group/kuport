package agent

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

// staleClaimAge is how long a link claim may sit unused before any selecting
// agent may drop it. It mirrors the window Compute uses when it proposes drops.
const staleClaimAge = 24 * time.Hour

// writeStatuses writes every piece of status this node owns: the PortMaps whose
// endpoint it holds, and its contribution to each class it participates in. A
// write failure on one object does not abandon the rest.
func (r *Reconciler) writeStatuses(ctx context.Context, res kreconcile.Result, hs hostState) error {
	var firstErr error
	for key, st := range res.PortMapStatus {
		if err := r.writePortMap(ctx, key, st); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for name, contrib := range res.ClassStatus {
		if err := r.writeClass(ctx, name, contrib, hs); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// writePortMap writes one PortMap's status under optimistic concurrency. This is
// the whole single-writer mechanism: on a conflict, RetryOnConflict re-runs the
// closure, which re-reads the object fresh and recomputes the update from it.
// The update must never be captured outside the closure and replayed, or a
// concurrent write to a field this node does not own is silently clobbered.
func (r *Reconciler) writePortMap(ctx context.Context, key types.NamespacedName, desired v1alpha1.PortMapStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var pm v1alpha1.PortMap
		if err := r.Client.Get(ctx, key, &pm); err != nil {
			if kerrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		next := pm.DeepCopy()
		next.Status.Endpoint = desired.Endpoint
		next.Status.Published = r.fillPublished(ctx, pm.Spec.ClassName, desired.Published)
		next.Status.ObservedGeneration = desired.ObservedGeneration
		for _, c := range desired.Conditions {
			meta.SetStatusCondition(&next.Status.Conditions, c)
		}

		if equality.Semantic.DeepEqual(pm.Status, next.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, next)
	})
}

// fillPublished resolves the address of each published row. A row naming this
// node is read off the host, where the interface lives. A row naming another
// accepting node is resolved from that node's own report: every accepting
// agent publishes the addresses of the class's interfaces on its node into its
// class status row, and the writer reads them from there. A row whose node has
// not reported yet keeps the empty address Compute produced and fills in on a
// later pass.
func (r *Reconciler) fillPublished(ctx context.Context, className string, rows []v1alpha1.PublishedAddress) []v1alpha1.PublishedAddress {
	if len(rows) == 0 {
		return rows
	}
	out := make([]v1alpha1.PublishedAddress, len(rows))
	copy(out, rows)

	needClass := false
	for _, row := range out {
		if row.Address == "" && row.Node != r.NodeName {
			needClass = true
			break
		}
	}
	var class v1alpha1.PortMapClass
	if needClass {
		if err := r.Client.Get(ctx, types.NamespacedName{Name: className}, &class); err != nil {
			// The class is gone or unreadable: leave remote rows blank this
			// pass rather than fail the write over them.
			class = v1alpha1.PortMapClass{}
		}
	}

	for i := range out {
		if out[i].Address != "" {
			continue
		}
		if out[i].Node == r.NodeName {
			if addr, err := r.Host.InterfaceAddr(out[i].Interface); err == nil {
				out[i].Address = addr
			}
			continue
		}
		for _, nr := range class.Status.Nodes {
			if nr.Name != out[i].Node {
				continue
			}
			if addr, ok := nr.Addresses[out[i].Interface]; ok {
				out[i].Address = addr
			}
			break
		}
	}
	return out
}

// interfaceAddrs resolves each of the class's interfaces to its address on
// this node. An interface with no resolvable address is left out, so the
// published row for it stays blank rather than claiming a wrong one.
func (r *Reconciler) interfaceAddrs(interfaces []string) map[string]string {
	if len(interfaces) == 0 {
		return nil
	}
	out := make(map[string]string, len(interfaces))
	for _, name := range interfaces {
		if addr, err := r.Host.InterfaceAddr(name); err == nil {
			out[name] = addr
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// writeClass writes this node's contribution to one class under the same
// optimistic-concurrency discipline as writePortMap: re-read fresh, recompute
// from the fresh object, write only the entries this node owns.
//
// The claim, drop and condition merges all act on the freshly read Links and
// Conditions, so two agents touching different rows never clobber each other,
// and a slot another agent has already taken is left alone rather than
// overwritten.
func (r *Reconciler) writeClass(ctx context.Context, name string, contrib kreconcile.ClassContribution, hs hostState) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var class v1alpha1.PortMapClass
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, &class); err != nil {
			if kerrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		next := class.DeepCopy()

		if contrib.Node.Name != "" {
			row := contrib.Node
			row.UnderlayMTU = int32(hs.underlayMTU)
			row.LinkMTU = int32(hs.linkMTU)
			row.Addresses = r.interfaceAddrs(class.Spec.Interfaces)
			upsertNodeRow(&next.Status.Nodes, row)
		}

		for _, claim := range contrib.ClaimLinks {
			mergeClaim(&next.Status.Links, claim)
		}
		for _, key := range contrib.DropLinks {
			dropStaleClaim(&next.Status.Links, key, r.now())
		}

		for _, c := range overlayConditions(contrib.Conditions, &class, hs, r.now()) {
			meta.SetStatusCondition(&next.Status.Conditions, c)
		}
		if next.Generation != 0 {
			next.Status.ObservedGeneration = next.Generation
		}

		if equality.Semantic.DeepEqual(class.Status, next.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, next)
	})
}

// upsertNodeRow inserts or replaces this node's row, matched by name.
func upsertNodeRow(rows *[]v1alpha1.NodeStatus, row v1alpha1.NodeStatus) {
	for i := range *rows {
		if (*rows)[i].Name == row.Name {
			(*rows)[i] = row
			return
		}
	}
	*rows = append(*rows, row)
}

// mergeClaim records a link claim without ever clobbering another agent's. A
// claim whose key already exists is left in place, its UnusedSince cleared
// because the pair is needed again. A claim for a new key whose slot a different
// key already holds is dropped this pass rather than overwritten: the claim was
// computed against a status this write has since found stale, and the next
// level-driven reconcile recomputes it against the fresh allocation.
func mergeClaim(links *[]v1alpha1.LinkAllocation, claim v1alpha1.LinkAllocation) {
	for i := range *links {
		if (*links)[i].Key == claim.Key {
			if (*links)[i].UnusedSince != nil {
				(*links)[i].UnusedSince = nil
			}
			return
		}
	}
	for i := range *links {
		if (*links)[i].Slot == claim.Slot {
			return // slot taken by another pair; recompute next pass.
		}
	}
	*links = append(*links, claim)
}

// dropStaleClaim removes a claim only if, in the freshly read object, it is
// still unused for more than 24h. Compute proposed the drop against a possibly
// stale status; re-checking here keeps a lost race harmless.
func dropStaleClaim(links *[]v1alpha1.LinkAllocation, key string, now metav1.Time) {
	out := (*links)[:0]
	for _, la := range *links {
		stale := la.Key == key && la.UnusedSince != nil &&
			now.Sub(la.UnusedSince.Time) > staleClaimAge
		if !stale {
			out = append(out, la)
		}
	}
	*links = out
}

// overlayConditions layers the host-only class conditions over the ones Compute
// produced. Compute cannot read the CNI's mode or the underlay, so the tunnel
// and port-collision refusals are decided here and take precedence over the
// Ready condition Compute set. None-mode classes build no return links, so
// neither host condition applies to them.
func overlayConditions(base []metav1.Condition, class *v1alpha1.PortMapClass, hs hostState, now metav1.Time) []metav1.Condition {
	if class.Spec.ReturnPath.Mode != v1alpha1.ReturnPathVxlan {
		return base
	}

	var reason, msg string
	switch {
	case hs.tunnelKnown && !hs.tunnelOK:
		reason = v1alpha1.ReasonTunnelModeRequired
		msg = "the CNI is not in tunnel mode; native routing drops the inbound leg"
	case classVxlanPort(class) == hs.cniPort:
		reason = v1alpha1.ReasonVxlanPortConflict
		msg = "return-path VXLAN port collides with the CNI's tunnel port"
	default:
		return base
	}

	out := make([]metav1.Condition, 0, len(base)+1)
	for _, c := range base {
		if c.Type != v1alpha1.ConditionReady {
			out = append(out, c)
		}
	}
	out = append(out, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: class.Generation,
	})
	return out
}

// classVxlanPort is the return-path VXLAN port a class asks for, defaulting to
// 4790 when the vxlan block leaves it unset.
func classVxlanPort(class *v1alpha1.PortMapClass) int {
	if v := class.Spec.ReturnPath.Vxlan; v != nil && v.Port != 0 {
		return int(v.Port)
	}
	return 4790
}
