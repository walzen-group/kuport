package reconcile

import (
	"fmt"
	"net/netip"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// buildClassStatus fills in this node's contribution to every class it
// participates in: its own Nodes row, the link claims it proposes or garbage
// collects, and the class-level conditions. A class participates when it selects
// this node. The row carries a real readiness verdict: every interface the class
// selects must resolve to an address on this node, and the first that does not
// is named in the row's message rather than silently dropped.
func buildClassStatus(res *Result, in Inputs, idx *index, mappings []*mapping, alloc map[string]classAlloc, needed map[string]map[string]neededPair) {
	for _, class := range in.Classes {
		accepting := idx.acceptingNodesFor(class)
		isAccepting := contains(accepting, in.NodeName)
		isHolder := holdsEndpointFor(needed[class.Name], in.NodeName)
		// A node contributes to a class it either accepts for (its Nodes row, GC
		// and conditions) or holds a return-link endpoint for (a fresh claim,
		// the same single-writer rule that governs PortMap status).
		if !isAccepting && !isHolder {
			continue
		}

		a := alloc[class.Name]
		c := ClassContribution{}

		if isAccepting {
			c.Node = nodeRowFor(in, class)
			// GC: any selecting agent clears, sets or drops UnusedSince.
			updates, drops := gcClaims(class, needed[class.Name], in.Now)
			c.ClaimLinks = append(c.ClaimLinks, updates...)
			c.DropLinks = append(c.DropLinks, drops...)
			c.Conditions = classConditions(a, in.Now, class.Generation)
		}

		// New claims: only the endpoint holder proposes one, and only for a pair
		// that has no existing claim.
		for key, slot := range a.newly {
			np := a.needed[key]
			if !np.holders[in.NodeName] {
				continue
			}
			lo, _ := nthSlash31(a.subnet, slot)
			c.ClaimLinks = append(c.ClaimLinks, v1alpha1.LinkAllocation{
				Key:    key,
				Peers:  []string{np.peers[0], np.peers[1]},
				Subnet: netip.PrefixFrom(lo, 31).String(),
				Slot:   int32(slot),
			})
		}

		sort.Slice(c.ClaimLinks, func(i, j int) bool { return c.ClaimLinks[i].Key < c.ClaimLinks[j].Key })
		sort.Strings(c.DropLinks)

		res.ClassStatus[class.Name] = c
	}
}

// holdsEndpointFor reports whether the node holds a chosen endpoint on any of a
// class's needed return-link pairs.
func holdsEndpointFor(needed map[string]neededPair, nodeName string) bool {
	for _, np := range needed {
		if np.holders[nodeName] {
			return true
		}
	}
	return false
}

// classConditions reports a class's Ready condition: false when its subnet fails
// to parse or runs out of slots, true otherwise.
func classConditions(a classAlloc, now metav1.Time, generation int64) []metav1.Condition {
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonValid,
		LastTransitionTime: now,
		ObservedGeneration: generation,
	}
	switch {
	case a.invalidSubnet:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonInvalidSubnet
		cond.Message = "return-path subnet cannot be parsed as an IPv4 CIDR"
	case a.exhausted:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonSubnetExhausted
		cond.Message = "no free /31 slot remains in the return-path subnet"
	}
	return []metav1.Condition{cond}
}

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

// nodeRowFor builds this node's class row: ready only when every interface
// the class selects resolves to an address here, per the host read handed in
// through Inputs. The message names the first interface that does not, in
// the shape spec.md documents. Addresses carries the interfaces that do
// resolve, the report the PortMap status writer reads for rows naming this
// node; an unresolved interface is absent there and in the message.
func nodeRowFor(in Inputs, class *v1alpha1.PortMapClass) v1alpha1.NodeStatus {
	row := v1alpha1.NodeStatus{Name: in.NodeName, Ready: true}
	for _, iface := range class.Spec.Interfaces {
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
	for _, iface := range interfacesOf(m.class) {
		rows = append(rows, v1alpha1.PublishedAddress{
			Node:      m.serving,
			Interface: iface,
			Address:   "",
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Interface < rows[j].Interface })
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
