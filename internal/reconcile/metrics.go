package reconcile

import (
	"fmt"
	"net/netip"
	"sort"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// Owner is the PortMap a DNAT entry serves.
type Owner struct{ Namespace, Name string }

// EgressOwner is the Egress and pod an egress entry serves.
type EgressOwner struct {
	Namespace, Egress, Pod string
	Blocked                bool
}

// MappingInfo describes a mapping this node holds a DNAT rule for.
type MappingInfo struct{ Namespace, Name, Class, Mode, Protocol, Port, EndpointNode string }

// MetricsIndex says which object each entry of this node's State serves, so the
// agent can label a counter with the PortMap or Egress it belongs to. The State
// itself carries no owner.
type MetricsIndex struct {
	DNAT     map[datapath.DNATKey]Owner
	Egress   map[netip.Addr]EgressOwner
	Mappings []MappingInfo // one per mapping with a DNAT rule on this node, sorted
}

// buildMetricsIndex runs the same emit functions buildState runs, one object at
// a time into a scratch State, and records which object produced each entry.
// It changes nothing buildState reads or writes.
func buildMetricsIndex(in Inputs, idx *index, mappings []*mapping, egresses []*egressDecision, alloc linkAlloc) MetricsIndex {
	out := MetricsIndex{DNAT: map[datapath.DNATKey]Owner{}, Egress: map[netip.Addr]EgressOwner{}}
	for _, m := range mappings {
		if !m.accepted || m.endpoint == nil || m.effAccepting == "" {
			continue
		}
		var st datapath.State
		emitMapping(&st, in, idx, m, alloc)
		if len(st.DNAT) == 0 {
			continue
		}
		owner := Owner{Namespace: m.pm.Namespace, Name: m.pm.Name}
		for _, d := range st.DNAT {
			out.DNAT[datapath.KeyOf(d)] = owner
		}
		out.Mappings = append(out.Mappings, mappingInfo(m))
	}
	for _, d := range egresses {
		if !d.ok {
			continue
		}
		for _, p := range d.pods {
			if p.path == v1alpha1.EgressPathLocal || p.node != in.NodeName {
				continue
			}
			var st datapath.State
			emitEgressPodNode(&st, in, idx, d, p, map[datapath.Family]bool{})
			for _, e := range st.Egress {
				out.Egress[e.SrcAddr] = EgressOwner{
					Namespace: d.eg.Namespace,
					Egress:    d.eg.Name,
					Pod:       p.name,
					Blocked:   e.Mark == datapath.EgressBlockedMark,
				}
			}
		}
	}
	sort.Slice(out.Mappings, func(i, j int) bool {
		a, b := out.Mappings[i], out.Mappings[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return out
}

func mappingInfo(m *mapping) MappingInfo {
	mode := string(m.class.Spec.ServingMode)
	if mode == "" {
		mode = string(v1alpha1.ServingSingle)
	}
	sel := portSel(m.pm)
	port := fmt.Sprint(sel.First)
	if sel.Last != sel.First {
		port = fmt.Sprintf("%d-%d", sel.First, sel.Last)
	}
	return MappingInfo{
		Namespace:    m.pm.Namespace,
		Name:         m.pm.Name,
		Class:        m.class.Name,
		Mode:         mode,
		Protocol:     sel.Proto,
		Port:         port,
		EndpointNode: m.endpoint.node,
	}
}
