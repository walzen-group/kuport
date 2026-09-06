package reconcile

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// resolveMappings turns every PortMap into a decision. It runs validation in the
// order the spec fixes, resolves the winner of each port conflict with a
// tie-break every agent computes identically, then chooses each accepted
// mapping's endpoint and the role this node plays. The returned slice is sorted
// by namespace/name so nothing downstream depends on input order.
func resolveMappings(in Inputs, idx *index) []*mapping {
	out := make([]*mapping, 0, len(in.PortMaps))
	byPM := make(map[*v1alpha1.PortMap]*mapping, len(in.PortMaps))
	for _, pm := range in.PortMaps {
		m := &mapping{pm: pm, class: idx.classByName[pm.Spec.ClassName]}
		if m.class != nil {
			m.acceptingNodes = idx.acceptingNodesFor(m.class)
		}
		out = append(out, m)
		byPM[pm] = m
	}

	// Conflict resolution needs every class's mappings in the same order on
	// every agent. Do the per-mapping checks first, then walk each class's
	// mappings earliest-first and let the earlier holder win.
	resolveConflicts(in, idx, byPM)

	// Endpoints and roles depend on the accepted set being settled.
	for _, m := range out {
		if m.accepted {
			m.endpoint = chooseEndpoint(in, m)
			decideRole(in, m)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return pmKey(out[i].pm) < pmKey(out[j].pm)
	})
	return out
}

// resolveConflicts fills in each mapping's accepted flag and Accepted condition.
// Checks 1..5 are independent per mapping; the conflict check (6) depends on the
// ordering, so the accepted set is built incrementally in earliest-first order:
// only a mapping that itself passed every check holds its interval against a
// later one.
func resolveConflicts(in Inputs, idx *index, byPM map[*v1alpha1.PortMap]*mapping) {
	// Group the mappings by class name and order each group.
	byClass := map[string][]*mapping{}
	for _, m := range byPM {
		byClass[m.pm.Spec.ClassName] = append(byClass[m.pm.Spec.ClassName], m)
	}

	for _, group := range byClass {
		sort.Slice(group, func(i, j int) bool { return earlier(group[i].pm, group[j].pm) })

		var held []*mapping // accepted mappings, in order, that hold an interval
		for _, m := range group {
			reason, msg, ok := validateStandalone(in, idx, m)
			if !ok {
				m.setAccepted(false, reason, msg, in.Now)
				continue
			}
			if winner := firstOverlap(m, held); winner != nil {
				m.setAccepted(false, v1alpha1.ReasonPortConflict,
					fmt.Sprintf("%s is held by %s", portPhrase(m.pm), pmKey(winner.pm)), in.Now)
				continue
			}
			m.setAccepted(true, v1alpha1.ReasonValid, "", in.Now)
			held = append(held, m)
		}
	}
}

// validateStandalone runs the per-mapping checks that do not depend on other
// mappings, in the spec's order. The first failure wins.
func validateStandalone(in Inputs, idx *index, m *mapping) (reason, msg string, ok bool) {
	pm := m.pm

	if m.class == nil {
		return v1alpha1.ReasonClassNotFound,
			fmt.Sprintf("class %q not found", pm.Spec.ClassName), false
	}

	first, last := interval(pm)
	if last < first {
		return v1alpha1.ReasonInvalidPortRange,
			fmt.Sprintf("endPort %d is less than port %d", last, first), false
	}

	if !classSelectsNamespace(idx, m.class, pm.Namespace) {
		return v1alpha1.ReasonNamespaceNotSelected,
			fmt.Sprintf("class %q does not select namespace %q", m.class.Name, pm.Namespace), false
	}

	min, max, reserved := portBounds(m.class)
	if first < min || last > max {
		return v1alpha1.ReasonPortOutOfRange,
			fmt.Sprintf("port range %d-%d is outside %d-%d", first, last, min, max), false
	}
	for _, r := range reserved {
		if r >= first && r <= last {
			return v1alpha1.ReasonPortReserved,
				fmt.Sprintf("port %d is reserved", r), false
		}
	}

	return v1alpha1.ReasonValid, "", true
}

// firstOverlap returns the earliest held mapping whose interval overlaps m on the
// same protocol, or nil when none does.
func firstOverlap(m *mapping, held []*mapping) *mapping {
	mf, ml := interval(m.pm)
	for _, h := range held {
		if h.pm.Spec.Protocol != m.pm.Spec.Protocol {
			continue
		}
		hf, hl := interval(h.pm)
		if mf <= hl && hf <= ml {
			return h
		}
	}
	return nil
}

// classSelectsNamespace reports whether a class's namespaceSelector selects a
// namespace. A nil or empty selector selects every namespace.
func classSelectsNamespace(idx *index, class *v1alpha1.PortMapClass, namespace string) bool {
	if class.Spec.NamespaceSelector == nil {
		return true
	}
	sel, err := metav1.LabelSelectorAsSelector(class.Spec.NamespaceSelector)
	if err != nil {
		return false
	}
	if sel.Empty() {
		return true
	}
	ns := idx.nsByName[namespace]
	if ns == nil {
		return false
	}
	return sel.Matches(labels.Set(ns.Labels))
}

// setAccepted records a mapping's Accepted decision as a condition.
func (m *mapping) setAccepted(ok bool, reason, msg string, now metav1.Time) {
	m.accepted = ok
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	m.acceptedCond = metav1.Condition{
		Type:               v1alpha1.ConditionAccepted,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: m.pm.Generation,
	}
}

// earlier reports whether a sorts before b: creationTimestamp ascending, ties
// broken by namespace/name ascending, bytewise. Creation timestamps have
// one-second resolution, so the tie-break is load-bearing, not decorative.
func earlier(a, b *v1alpha1.PortMap) bool {
	at, bt := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return pmKey(a) < pmKey(b)
}

// pmKey is a PortMap's namespace/name, the bytewise tie-break key.
func pmKey(pm *v1alpha1.PortMap) string {
	return pm.Namespace + "/" + pm.Name
}

// interval returns the inclusive [first, last] port interval a mapping asks for.
// An absent endPort makes the interval the single port.
func interval(pm *v1alpha1.PortMap) (first, last int32) {
	first = pm.Spec.Port
	last = pm.Spec.Port
	if pm.Spec.EndPort != nil {
		last = *pm.Spec.EndPort
	}
	return
}

// portBounds returns a class's port bounds. An absent ports block means 1..65535
// with nothing reserved.
func portBounds(class *v1alpha1.PortMapClass) (min, max int32, reserved []int32) {
	min, max = 1, 65535
	if p := class.Spec.Ports; p != nil {
		if p.Min != 0 {
			min = p.Min
		}
		if p.Max != 0 {
			max = p.Max
		}
		reserved = p.Reserved
	}
	return
}

// portPhrase renders a mapping's interval for a conflict message: "port 3000"
// for a single port, "ports 3000-3005" for a range.
func portPhrase(pm *v1alpha1.PortMap) string {
	first, last := interval(pm)
	if first == last {
		return fmt.Sprintf("port %d", first)
	}
	return fmt.Sprintf("ports %d-%d", first, last)
}
