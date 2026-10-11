package agent

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

// writeStatuses writes every piece of status this node owns: the PortMaps whose
// endpoint it holds, its contribution to each class it accepts for, and its
// writes to the Link pool. A write failure on one object does not abandon the
// rest.
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
	for key, st := range res.EgressStatus {
		if err := r.writeEgress(ctx, key, st); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for name, contrib := range res.EgressClassStatus {
		if err := r.writeEgressClass(ctx, name, contrib, hs); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := r.writeLinks(ctx, res.Links); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// writeEgress writes the status of an Egress this node owns, under the
// discipline writePortMap follows: re-read, merge, write only on a change.
func (r *Reconciler) writeEgress(ctx context.Context, key types.NamespacedName, desired v1alpha1.EgressStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var eg v1alpha1.Egress
		if err := r.Client.Get(ctx, key, &eg); err != nil {
			if kerrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		next := eg.DeepCopy()
		next.Status.Node = desired.Node
		next.Status.Pods = desired.Pods
		next.Status.ObservedGeneration = desired.ObservedGeneration
		for _, c := range desired.Conditions {
			meta.SetStatusCondition(&next.Status.Conditions, c)
		}
		if equality.Semantic.DeepEqual(eg.Status, next.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, next)
	})
}

// writeEgressClass writes this node's row into an EgressClass, with the MTU
// figures read from the host, and the class conditions.
func (r *Reconciler) writeEgressClass(ctx context.Context, name string, contrib kreconcile.EgressClassContribution, hs hostState) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var class v1alpha1.EgressClass
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, &class); err != nil {
			if kerrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		next := class.DeepCopy()
		row := contrib.Node
		row.UnderlayMTU = int32(hs.underlayMTU)
		row.LinkMTU = int32(hs.linkMTU)
		upsertNodeRow(&next.Status.Nodes, row)
		for _, c := range contrib.Conditions {
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

// writeLinks makes this node's writes to the Link pool. Each one is the whole
// decision for its object, computed against the version this pass read:
//
//   - A create that finds the name taken returns AlreadyExists. Another agent
//     claimed the slot first, and the watch brings its Link into the next pass,
//     which picks another slot.
//   - A status update and a delete carry the version Compute saw. When the
//     object has moved on, the write conflicts and is dropped; the watch
//     delivers the newer object and the next pass decides again.
//
// Nothing here merges a computed value into a fresher object, so a decision is
// written as Compute made it or not at all.
func (r *Reconciler) writeLinks(ctx context.Context, acts kreconcile.LinkActions) error {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	for i := range acts.Create {
		l := acts.Create[i].DeepCopy()
		if err := r.Client.Create(ctx, l); err != nil && !kerrors.IsAlreadyExists(err) {
			keep(err)
		}
	}

	for _, u := range acts.Status {
		var l v1alpha1.Link
		if err := r.Client.Get(ctx, types.NamespacedName{Name: u.Name}, &l); err != nil {
			if !kerrors.IsNotFound(err) {
				keep(err)
			}
			continue
		}
		if l.ResourceVersion != u.ResourceVersion {
			continue
		}
		l.Status.UnusedSince = u.UnusedSince
		if err := r.Client.Status().Update(ctx, &l); err != nil && !kerrors.IsConflict(err) && !kerrors.IsNotFound(err) {
			keep(err)
		}
	}

	for _, d := range acts.Delete {
		rv := d.ResourceVersion
		l := &v1alpha1.Link{ObjectMeta: metav1.ObjectMeta{Name: d.Name}}
		err := r.Client.Delete(ctx, l, client.Preconditions{ResourceVersion: &rv})
		if err != nil && !kerrors.IsConflict(err) && !kerrors.IsNotFound(err) {
			keep(err)
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
		want6 := desired.Endpoint != nil && desired.Endpoint.Address6 != ""
		next.Status.Published = r.fillPublished(ctx, pm.Spec.ClassName, desired.Published, want6)
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
// later pass. With want6, set when the mapping is delivered over IPv6, each
// row's IPv6 address is resolved the same way.
func (r *Reconciler) fillPublished(ctx context.Context, className string, rows []v1alpha1.PublishedAddress, want6 bool) []v1alpha1.PublishedAddress {
	if len(rows) == 0 {
		return rows
	}
	out := make([]v1alpha1.PublishedAddress, len(rows))
	copy(out, rows)

	needClass := false
	for _, row := range out {
		if row.Node != r.NodeName && (row.Address == "" || (want6 && row.Address6 == "")) {
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
		if out[i].Node == r.NodeName {
			if out[i].Address == "" {
				if addr, err := r.Host.InterfaceAddr(out[i].Interface); err == nil {
					out[i].Address = addr
				}
			}
			if want6 && out[i].Address6 == "" {
				if addr, err := r.Host.InterfaceAddr6(out[i].Interface); err == nil {
					out[i].Address6 = addr
				}
			}
			continue
		}
		for _, nr := range class.Status.Nodes {
			if nr.Name != out[i].Node {
				continue
			}
			if addr, ok := nr.Addresses[out[i].Interface]; ok && out[i].Address == "" {
				out[i].Address = addr
			}
			if addr, ok := nr.Addresses6[out[i].Interface]; ok && want6 && out[i].Address6 == "" {
				out[i].Address6 = addr
			}
			break
		}
	}
	return out
}

// writeClass writes this node's contribution to one class under the same
// optimistic-concurrency discipline as writePortMap: re-read fresh, recompute
// from the fresh object, write only the entries this node owns. The row and
// condition merges act on the freshly read object, so two agents touching
// different rows never clobber each other.
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

		// The row arrives complete from Compute: name, readiness, the
		// message naming an unresolvable interface, and the addresses it
		// could resolve. Only the MTU numbers are decided here, since they
		// are read from the host after Compute ran.
		if contrib.Node.Name != "" {
			row := contrib.Node
			row.UnderlayMTU = int32(hs.underlayMTU)
			row.LinkMTU = int32(hs.linkMTU)
			upsertNodeRow(&next.Status.Nodes, row)
		}

		if contrib.ClearLegacyLinks {
			next.Status.Links = nil
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

// overlayConditions layers the host-only class conditions over the ones Compute
// produced. Compute cannot read the CNI's mode or the underlay, so the tunnel
// and port-collision refusals are decided here and take precedence over the
// Ready condition Compute set. None-mode classes build no return links, so
// neither host condition applies to them.
func overlayConditions(base []metav1.Condition, class *v1alpha1.PortMapClass, hs hostState, now metav1.Time) []metav1.Condition {
	if class.Spec.ReturnPath.Mode != v1alpha1.ReturnPathVxlan {
		return base
	}

	// The CNI's routing mode used to be checked here, because the inbound leg
	// rode the CNI's tunnel and native routing dropped it. Since v0.4.0 both
	// legs ride kuport's own return link, so the mode no longer matters. What
	// still does is the port: two VXLAN devices cannot share a VNI and port.
	if hs.linkPort != hs.cniPort {
		return base
	}
	reason := v1alpha1.ReasonVxlanPortConflict
	msg := "the agent's --link-port collides with the CNI's tunnel port"

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
