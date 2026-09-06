package reconcile

import (
	"fmt"
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
// accepting node by name. Exactly S programs the mapping; the adjudicated
// semantics have every other accepting node report the
// RemotePodMultipleAcceptingNodes refusal, computed identically here, and
// only S owns the status (see statusOwner). Programmed=True is deferred to
// resolveProgrammed for the single-accepting-node case, where it waits on
// the class node rows and, for a remote pod, the landed return-path slot.
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

	m.remote = m.serving != m.endpoint.node

	if m.remote && returnPathMode(m.class) == v1alpha1.ReturnPathNone {
		// Anchored on S: every agent computes the same S and the same
		// refusal, and no node programs a mapping whose replies cannot
		// return.
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonReturnPathUnavailable,
			"return path is None and the chosen pod is on another node", in.Now)
		return
	}

	// S is the sole programming node, local pod or remote.
	m.effAccepting = m.serving

	if len(m.acceptingNodes) >= 2 {
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonRemotePodMultipleAcceptingNodes,
			fmt.Sprintf("class selects %d accepting nodes; only %s programs this mapping",
				len(m.acceptingNodes), m.serving), in.Now)
		return
	}

	// Single accepting node: True or False is decided against the class
	// node rows and the return-path slot, both settled after allocation.
	m.gated = true
}

// returnPathMode returns a class's return-path mode.
func returnPathMode(class *v1alpha1.PortMapClass) v1alpha1.ReturnPathMode {
	return class.Spec.ReturnPath.Mode
}

// interfacesOf returns the interfaces a class programs DNAT rules on.
func interfacesOf(class *v1alpha1.PortMapClass) []string {
	return class.Spec.Interfaces
}

// nodeAddrOf returns a node's InternalIP from the index, or "" when unknown.
func nodeAddrOf(idx *index, name string) string {
	return idx.nodeAddr[name]
}
