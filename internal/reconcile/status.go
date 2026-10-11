package reconcile

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// buildClassStatus fills in this node's contribution to every class that
// accepts on it: its own Nodes row and the class-level conditions. The row
// carries a real readiness verdict: every interface the class selects must
// resolve to an address on this node, and the first that does not is named in
// the row's message rather than silently dropped.
func buildClassStatus(res *Result, in Inputs, idx *index, alloc linkAlloc) {
	for _, class := range in.Classes {
		if !contains(idx.acceptingNodesFor(class), in.NodeName) {
			continue
		}
		res.ClassStatus[class.Name] = ClassContribution{
			Node:             nodeRowFor(in, class),
			Conditions:       classConditions(class, alloc, in.Now),
			ClearLegacyLinks: len(class.Status.Links) > 0,
		}
	}
}

// classConditions reports a class's Ready condition, false when the link pool
// has no slot left for one of its pairs, and its Deprecated condition, true
// when it sets a returnPath.vxlan field kuport ignores.
func classConditions(class *v1alpha1.PortMapClass, alloc linkAlloc, now metav1.Time) []metav1.Condition {
	ready := metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonValid,
		LastTransitionTime: now,
		ObservedGeneration: class.Generation,
	}
	if alloc.exhausted[linkClass{kind: v1alpha1.ClassKindPortMapClass, name: class.Name}] {
		ready.Status = metav1.ConditionFalse
		ready.Reason = v1alpha1.ReasonSubnetExhausted
		ready.Message = fmt.Sprintf("all %d slots of the cluster-wide link pool are taken", linkSlots)
	}

	deprecated := metav1.Condition{
		Type:               v1alpha1.ConditionDeprecated,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonNoDeprecated,
		LastTransitionTime: now,
		ObservedGeneration: class.Generation,
	}
	if fields := deprecatedVxlanFields(class); len(fields) > 0 {
		deprecated.Status = metav1.ConditionTrue
		deprecated.Reason = v1alpha1.ReasonDeprecatedField
		deprecated.Message = fmt.Sprintf("%s set but ignored: every link takes its numbers from its Link object's slot; remove the fields",
			strings.Join(fields, ", "))
	}

	conds := []metav1.Condition{deprecated, ready}
	sortConditions(conds)
	return conds
}

// ipv6ForwardingOff is the node row's ipv6Unavailable message on a host that
// does not forward IPv6.
const ipv6ForwardingOff = "net.ipv6.conf.all.forwarding is 0 on this node, so it delivers no mapping over IPv6"

// buildPortMapStatus emits the status for every PortMap this node owns. Exactly
// one node owns each PortMap, so exactly one agent writes it: the mapping's
// serving node, the shared choice every agent computes. A refused or
// endpoint-less mapping still reports its conditions from there.
func buildPortMapStatus(res *Result, in Inputs, mappings []*mapping) {
	for _, m := range mappings {
		if statusOwner(in, m) != in.NodeName {
			continue
		}

		st := v1alpha1.PortMapStatus{
			ObservedGeneration: m.pm.Generation,
			Conditions:         []metav1.Condition{m.acceptedCond},
		}
		if m.hasProgrammed {
			st.Conditions = append(st.Conditions, m.programmedCond)
		}
		sortConditions(st.Conditions)

		if m.endpoint != nil {
			st.Endpoint = &v1alpha1.Endpoint{Node: m.endpoint.node, Address: m.endpoint.addr}
			// The status writer reports the IPv6 address when it delivers
			// IPv6 itself; the agent fills the published IPv6 addresses only
			// then.
			if pods := podAddrs(in, m); len(pods) > 1 {
				st.Endpoint.Address6 = pods[1].String()
			}
			st.Published = publishedRows(m)
		}

		res.PortMapStatus[types.NamespacedName{Namespace: m.pm.Namespace, Name: m.pm.Name}] = st
	}
}

// statusOwner is the single node responsible for writing a PortMap's status.
// It is the mapping's serving node, the one accepted choice every agent
// computes identically. With no serving node — nothing accepts, or the
// mapping was never accepted — the chosen endpoint's node reports; with no
// endpoint either, the first accepting node by name, else the first node in
// the cluster by name, so a refused or endpoint-less mapping still carries
// its conditions. Exactly one writer per PortMap in every case, without
// leader election.
func statusOwner(in Inputs, m *mapping) string {
	if m.serving != "" {
		return m.serving
	}
	if m.endpoint != nil {
		return m.endpoint.node
	}
	if len(m.acceptingNodes) > 0 {
		return m.acceptingNodes[0]
	}
	names := make([]string, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// nodeRowFor builds this node's class row: ready only when every interface the
// class gives this node resolves to an address here, per the host read handed
// in through Inputs. The class names each node's interfaces, so a name that
// does not resolve is a mistake in the class rather than a node that lacks the
// NIC. The message names the first interface that does not, in the shape
// spec.md documents. Addresses carries the interfaces that do resolve, the
// report the PortMap status writer reads for rows naming this node; an
// unresolved interface is absent there and in the message. Addresses6 does the
// same for IPv6 while this node forwards it. When it does not, and one of the
// interfaces holds an IPv6 address an IPv6 client could dial, IPv6Unavailable
// says why that client gets nothing. Neither touches readiness, since IPv4
// works either way.
func nodeRowFor(in Inputs, class *v1alpha1.PortMapClass) v1alpha1.NodeStatus {
	row := v1alpha1.NodeStatus{Name: in.NodeName, Ready: true}
	for _, iface := range classInterfacesOn(class, in.NodeName) {
		if addr6, ok := in.InterfaceAddrs6[iface]; ok {
			if !in.IPv6Forwarding {
				row.IPv6Unavailable = ipv6ForwardingOff
			} else {
				if row.Addresses6 == nil {
					row.Addresses6 = map[string]string{}
				}
				row.Addresses6[iface] = addr6
			}
		}
		addr, ok := in.InterfaceAddrs[iface]
		if !ok {
			row.Ready = false
			if row.Message == "" {
				row.Message = fmt.Sprintf("interface %s not present", iface)
			}
			continue
		}
		if row.Addresses == nil {
			row.Addresses = map[string]string{}
		}
		row.Addresses[iface] = addr
	}
	return row
}

// classNodeRow returns one node's row from a class's status, or nil when the
// node has not reported one.
func classNodeRow(class *v1alpha1.PortMapClass, name string) *v1alpha1.NodeStatus {
	for i := range class.Status.Nodes {
		if class.Status.Nodes[i].Name == name {
			return &class.Status.Nodes[i]
		}
	}
	return nil
}

// publishedRows lists the addresses a mapping answers on. Under the
// 2026-09-06 adjudication that is exactly the serving node S's interfaces:
// the only places the DNAT rules exist, the only addresses where the port
// answers. An accepting sibling serves nothing and appears nowhere. The
// addresses are left empty here; the agent fills them from the host for
// this node and from the reported row for others (fillPublished).
func publishedRows(m *mapping) []v1alpha1.PublishedAddress {
	if m.serving == "" {
		return nil
	}
	var rows []v1alpha1.PublishedAddress
	for _, node := range m.programmers {
		for _, iface := range interfacesFor(m.class, node, m.pm) {
			rows = append(rows, v1alpha1.PublishedAddress{
				Node:      node,
				Interface: iface,
				Address:   "",
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Node != rows[j].Node {
			return rows[i].Node < rows[j].Node
		}
		return rows[i].Interface < rows[j].Interface
	})
	return rows
}

// sortConditions orders conditions by type so status is stable across passes.
func sortConditions(cs []metav1.Condition) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].Type < cs[j].Type })
}

// contains reports whether s holds v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
