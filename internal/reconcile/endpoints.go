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
// the choice the spec fixes: a candidate on this node wins, otherwise the first
// by targetRef.name ascending. That second rule, not slice order, is what makes
// every agent pick the same endpoint. It returns nil when there is no candidate.
func chooseEndpoint(in Inputs, idx *index, m *mapping) *chosenEndpoint {
	pm := m.pm
	candidates := gatherCandidates(in, pm)
	if len(candidates) == 0 {
		return nil
	}

	// Sort by targetRef.name, bytewise, so the fallback and the local-preference
	// tie-break are both stable across agents and watch orderings.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].targetR < candidates[j].targetR
	})

	// A candidate on this node wins; the lowest targetRef.name among local
	// candidates, since the slice is already sorted.
	for i := range candidates {
		if candidates[i].node == in.NodeName {
			return &candidates[i]
		}
	}
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

// decideRole fixes the role this node plays for a mapping and its Programmed
// condition, applying the two refusals in precedence order: a None return path
// with a remote pod is unavailable, and several accepting nodes with a remote
// pod is the documented single-node limitation. Both leave the mapping with no
// rules on the nodes they refuse.
func decideRole(in Inputs, idx *index, m *mapping) {
	if m.endpoint == nil {
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonNoReadyEndpoint,
			"no ready endpoint for the named service port", in.Now)
		return
	}

	endpointNode := m.endpoint.node
	accepting := m.acceptingNodes

	if len(accepting) == 0 {
		// The class selects no node, so nothing can accept the port.
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonNodeNotReady,
			"class selects no accepting node", in.Now)
		return
	}

	// Co-located on a single accepting node: DNAT only, no return link.
	if len(accepting) == 1 && accepting[0] == endpointNode {
		m.effAccepting = accepting[0]
		m.remote = false
		m.setProgrammed(metav1.ConditionTrue, v1alpha1.ReasonAllNodesReady, "", in.Now)
		return
	}

	mode := returnPathMode(m.class)
	if mode == v1alpha1.ReturnPathNone {
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonReturnPathUnavailable,
			"return path is None and the chosen pod is on another node", in.Now)
		return
	}

	if len(accepting) >= 2 {
		first := accepting[0]
		m.effAccepting = first
		m.remote = first != endpointNode
		m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonRemotePodMultipleAcceptingNodes,
			"class selects several accepting nodes and the pod is remote; only "+first+" is programmed", in.Now)
		return
	}

	// Single accepting node, pod elsewhere: forward across a return link.
	m.effAccepting = accepting[0]
	m.remote = true
	m.setProgrammed(metav1.ConditionTrue, v1alpha1.ReasonAllNodesReady, "", in.Now)
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
