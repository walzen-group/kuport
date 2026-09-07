package reconcile

import (
	"sort"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// serviceNameLabel is the well-known label EndpointSlices carry to name their
// Service.
const serviceNameLabel = "kubernetes.io/service-name"

// chooseEndpoint resolves a mapping's serviceRef to a single endpoint. It
// gathers the ready IPv4 candidates behind the named Service port, then applies
// the choice every agent computes to the same answer from shared inputs: a
// candidate on an accepting node wins, ranked by accepting-node name
// ascending, then by targetRef.name ascending within that node; with no
// candidate on an accepting node, the first by targetRef.name overall. There is
// no node-local preference: two agents picking different endpoints program
// different pods. It returns nil when there is no candidate.
func chooseEndpoint(in Inputs, m *mapping) *chosenEndpoint {
	pm := m.pm
	candidates := gatherCandidates(in, pm)
	if len(candidates) == 0 {
		return nil
	}

	// acceptingRank orders nodes by name for the primary key; a node the
	// class does not select ranks after every accepting node, so the sort's
	// tail is the targetRef.name fallback.
	acceptingRank := func(node string) int {
		for i, n := range m.acceptingNodes {
			if n == node {
				return i
			}
		}
		return len(m.acceptingNodes)
	}
	sort.Slice(candidates, func(i, j int) bool {
		ri, rj := acceptingRank(candidates[i].node), acceptingRank(candidates[j].node)
		if ri != rj {
			return ri < rj
		}
		if candidates[i].targetR != candidates[j].targetR {
			return candidates[i].targetR < candidates[j].targetR
		}
		if candidates[i].node != candidates[j].node {
			return candidates[i].node < candidates[j].node
		}
		return candidates[i].addr < candidates[j].addr
	})
	return &candidates[0]
}

// gatherCandidates returns every ready IPv4 endpoint behind the mapping's named
// Service port, in arbitrary order.
func gatherCandidates(in Inputs, pm *v1alpha1.PortMap) []chosenEndpoint {
	var out []chosenEndpoint
	for _, sl := range in.Slices {
		if sl.Namespace != pm.Namespace {
			continue
		}
		if sl.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		if sl.Labels[serviceNameLabel] != pm.Spec.ServiceRef.Name {
			continue
		}
		if !sliceHasPort(sl, pm.Spec.ServiceRef.Port) {
			continue
		}
		for i := range sl.Endpoints {
			ep := &sl.Endpoints[i]
			if !endpointReady(ep) {
				continue
			}
			if len(ep.Addresses) == 0 {
				continue
			}
			out = append(out, chosenEndpoint{
				node:    strFromPtr(ep.NodeName),
				addr:    ep.Addresses[0],
				targetR: targetRefName(ep),
			})
		}
	}
	return out
}

// sliceHasPort reports whether an EndpointSlice serves the named port.
func sliceHasPort(sl *discoveryv1.EndpointSlice, name string) bool {
	for _, p := range sl.Ports {
		if p.Name != nil && *p.Name == name {
			return true
		}
	}
	return false
}

// endpointReady reports whether an endpoint is ready. A nil Ready means ready,
// per the EndpointSlice contract.
func endpointReady(ep *discoveryv1.Endpoint) bool {
	return ep.Conditions.Ready == nil || *ep.Conditions.Ready
}

// targetRefName is the endpoint's pod name, or "" when it has no targetRef.
func targetRefName(ep *discoveryv1.Endpoint) string {
	if ep.TargetRef == nil {
		return ""
	}
	return ep.TargetRef.Name
}

func strFromPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// decideRole fixes the serving node for a mapping and this node's Programmed
// condition. The serving node S is a shared computation: the node holding the
// chosen endpoint when that node accepts the class, otherwise the first
// accepting node by name. Exactly S programs the mapping and S is the only
// node that writes the mapping's status (see statusOwner). Accepting nodes
// that are not S serve nothing: no rules, no status, and — per the 2026-09-06
// adjudication that a served mapping reports the truth about itself — no
// refusal. Programmed=True is deferred to resolveProgrammed, where it waits
// on the participants' readiness: S's class node row and, for a remote pod,
// the landed return-path slot.
func decideRole(in Inputs, m *mapping) {
	// The serving node derives from the shared choice, never from a
	// node-local preference. It stays "" only when no node accepts, where
	// the status-owner fallback chain takes over.
	if len(m.acceptingNodes) > 0 {
		m.serving = m.acceptingNodes[0]
		if m.endpoint != nil && contains(m.acceptingNodes, m.endpoint.node) {
			m.serving = m.endpoint.node
		}
	}

	if m.endpoint == nil {
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonNoReadyEndpoint,
			"no ready endpoint for the named service port", in.Now)
		return
	}

	if len(m.acceptingNodes) == 0 {
		// The class selects no node, so nothing can accept the port.
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonNodeNotReady,
			"class selects no accepting node", in.Now)
		return
	}

	// Under Multi every accepting node forwards the mapping, so a routed
	// virtual address reaching any of them works. Under Single only S does.
	// The status owner stays S either way, which keeps one writer per PortMap.
	if servingModeOf(m.class) == v1alpha1.ServingMulti {
		m.programmers = append([]string(nil), m.acceptingNodes...)
	} else {
		m.programmers = []string{m.serving}
	}

	m.remote = false
	for _, p := range m.programmers {
		if p != m.endpoint.node {
			m.remote = true
		}
	}

	if m.remote && returnPathMode(m.class) == v1alpha1.ReturnPathNone {
		// Anchored on S: every agent computes the same programmer set and the
		// same refusal, and no node programs a mapping whose replies cannot
		// return.
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonReturnPathUnavailable,
			"return path is None and the chosen pod is not on every programming node", in.Now)
		m.programmers = nil
		return
	}

	// S owns the status whichever mode is in force.
	m.effAccepting = m.serving

	// Whether the served mapping reads True is decided against the
	// participants' rows and the return-path slot, both settled after
	// allocation; sibling accepting nodes are outside the mapping and gate
	// nothing there.
	m.gated = true
}

// servingModeOf returns a class's serving mode, defaulting to Single so a
// class written before the field existed keeps one serving node.
func servingModeOf(class *v1alpha1.PortMapClass) v1alpha1.ServingMode {
	if class.Spec.ServingMode == v1alpha1.ServingMulti {
		return v1alpha1.ServingMulti
	}
	return v1alpha1.ServingSingle
}

// returnPathMode returns a class's return-path mode.
func returnPathMode(class *v1alpha1.PortMapClass) v1alpha1.ReturnPathMode {
	return class.Spec.ReturnPath.Mode
}

// classInterfacesOn returns the interfaces a class gives one node, sorted. A
// node the class does not name gets none.
func classInterfacesOn(class *v1alpha1.PortMapClass, node string) []string {
	ni, ok := class.Spec.Nodes[node]
	if !ok {
		return nil
	}
	out := append([]string(nil), ni.Interfaces...)
	sort.Strings(out)
	return out
}

// interfacesFor returns the interfaces one mapping programs DNAT rules on at
// one node: what the class gives that node, narrowed by the mapping's own list
// when it sets one. A name the mapping asks for that the node does not carry
// contributes nothing, which is what lets one list cover nodes whose NICs are
// named differently.
func interfacesFor(class *v1alpha1.PortMapClass, node string, pm *v1alpha1.PortMap) []string {
	have := classInterfacesOn(class, node)
	if pm == nil || len(pm.Spec.Interfaces) == 0 {
		return have
	}
	var out []string
	for _, iface := range have {
		if contains(pm.Spec.Interfaces, iface) {
			out = append(out, iface)
		}
	}
	return out
}

// nodeAddrOf returns a node's InternalIP from the index, or "" when unknown.
func nodeAddrOf(idx *index, name string) string {
	return idx.nodeAddr[name]
}
