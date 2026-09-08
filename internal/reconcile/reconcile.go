// Package reconcile computes, as a pure function, the complete desired state a
// single kuport agent should impose on its node. Every agent in the DaemonSet
// runs the same computation over the same watched inputs and, because every
// tie-break here is deterministic, they arrive at the same decisions without
// talking to each other. That agreement is a correctness property: two agents
// choosing different endpoints for one mapping program different pods and take
// the port down while every rule still looks right.
//
// Compute reads only its Inputs. It makes no API calls, reads no clock beyond
// the injected Now, touches no host, and starts no goroutines. Task 6 wraps it
// in informers and applies the State it returns; task 2 turns that State into
// nftables and netlink objects.
package reconcile

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// Inputs is everything Compute is allowed to see. It is the watched world as
// of one reconcile pass, plus the name of the node this pass computes for, a
// single injected timestamp, and the agent's host read of the interfaces the
// classes select. Nothing else reaches the computation.
type Inputs struct {
	NodeName   string
	Nodes      []*corev1.Node
	Namespaces []*corev1.Namespace
	Classes    []*v1alpha1.PortMapClass
	PortMaps   []*v1alpha1.PortMap
	Slices     []*discoveryv1.EndpointSlice
	Now        metav1.Time

	// InterfaceAddrs resolves interface names to this node's IPv4 addresses,
	// read from the host by the agent through its Host seam. An interface
	// absent from the map is missing or unaddressed here: Compute reports
	// the class's node row not-ready naming it, never a skipped row.
	InterfaceAddrs map[string]string
}

// Result is the desired state for this node plus the status this node is
// responsible for writing. It is a pure function of Inputs: the same Inputs
// yield the same Result, byte for byte, every pass.
type Result struct {
	// State is what this node's host should look like. It goes straight to
	// datapath.Apply.
	State datapath.State

	// PortMapStatus holds the status for the PortMaps this node owns, keyed by
	// namespace/name. Exactly one node owns each PortMap, so exactly one agent
	// writes it. Ownership follows the chosen endpoint and changes hands when
	// the endpoint moves.
	PortMapStatus map[types.NamespacedName]v1alpha1.PortMapStatus

	// ClassStatus is the per-class status this node contributes, keyed by class
	// name: its own Nodes row, and any Links entries it claims or releases.
	ClassStatus map[string]ClassContribution
}

// ClassContribution is one node's contribution to a class's status. The Node
// row is always present for a class this node participates in; the link fields
// carry the recorded-claim allocation and its garbage collection.
type ClassContribution struct {
	Node       v1alpha1.NodeStatus
	ClaimLinks []v1alpha1.LinkAllocation // to add or update
	DropLinks  []string                  // Key values to remove
	Conditions []metav1.Condition
}

// mapping is the fully-resolved decision for one PortMap: the outcome of
// validation, the chosen endpoint, and the role this node plays. It is the unit
// the State and status builders consume.
type mapping struct {
	pm    *v1alpha1.PortMap
	class *v1alpha1.PortMapClass

	// accepted is true when the mapping passed every validation check.
	accepted bool
	// acceptedCond is the Accepted condition to report.
	acceptedCond metav1.Condition

	// acceptingNodes is the sorted set of node names the class selects.
	acceptingNodes []string

	// endpoint is the chosen endpoint, nil when there is no ready candidate.
	endpoint *chosenEndpoint

	// serving is the one accepting node that programs this mapping, derived
	// from the shared endpoint choice: the endpoint's node when it accepts,
	// else the first accepting node by name. Every agent computes the same
	// value, and only it owns the mapping's status. It is "" when the class
	// selects no node or the mapping was not accepted.
	serving string

	// effAccepting is the node that actually forwards this mapping: serving
	// when the mapping is programmed anywhere, "" when no node may program
	// it (no endpoint, no accepting node, or the None-return-path refusal).
	// Under Single serving it is the only programmer; under Multi it stays the
	// status owner while programmers carries every node that forwards.
	effAccepting string
	// programmers is every node that writes this mapping's DNAT rules: just
	// effAccepting under Single, every accepting node under Multi. Empty when
	// no node may program the mapping. Sorted, so emission is deterministic.
	programmers []string
	// remote is true when a programmer must reach the pod across a return link.
	// Under Multi it is true when any programmer is off the pod's node, which
	// is what decides whether the class's return path has to exist at all.
	remote bool
	// gated marks the accepted, served mapping whose final Programmed
	// condition resolveProgrammed settles after the slot allocation is
	// known, on the participants only: S's row and, for a remote pod, the
	// landed S-holder return-path slot.
	gated bool

	// programmedCond is the Programmed condition to report, set for accepted
	// mappings only.
	programmedCond metav1.Condition
	// hasProgrammed reports whether programmedCond is set.
	hasProgrammed bool
}

// setProgrammed records a mapping's Programmed condition.
func (m *mapping) setProgrammed(status metav1.ConditionStatus, reason, msg string, now metav1.Time) {
	m.programmedCond = metav1.Condition{
		Type:               v1alpha1.ConditionProgrammed,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
		ObservedGeneration: m.pm.Generation,
	}
	m.hasProgrammed = true
}

// chosenEndpoint is the single endpoint a mapping delivers to.
type chosenEndpoint struct {
	node    string
	addr    string
	targetR string
}

// Compute produces the desired state and owned status for in.NodeName. It is
// pure: no API calls, no host access, no clock beyond in.Now, and every output
// slice is sorted so that map iteration order never leaks into the Result.
func Compute(in Inputs) Result {
	res := Result{
		PortMapStatus: map[types.NamespacedName]v1alpha1.PortMapStatus{},
		ClassStatus:   map[string]ClassContribution{},
	}

	idx := newIndex(in)

	// Resolve every PortMap to a decision: validation, conflict ordering,
	// endpoint choice and roles. conflicts.go owns the ordering.
	mappings := resolveMappings(in, idx)

	// Settle the return-link slot allocation per class, so State and status
	// agree on which slot every needed pair uses. links.go owns the arithmetic.
	needed := neededPairsByClass(mappings)
	alloc := map[string]classAlloc{}
	for _, c := range in.Classes {
		alloc[c.Name] = allocateClass(c, needed[c.Name])
	}

	// The served mappings deferred their Programmed condition to the
	// settled rows and slots; decideRole could not see either.
	resolveProgrammed(in, idx, mappings, alloc)

	// State: the rules, links, marks and routes this node should hold.
	buildState(&res.State, in, idx, mappings, alloc)

	// Class status: this node's Nodes row, link claims and GC, class
	// conditions.
	buildClassStatus(&res, in, idx, mappings, alloc, needed)

	// PortMap status: emitted only by the node that owns each mapping.
	buildPortMapStatus(&res, in, mappings)

	return res
}

// buildState assembles this node's complete desired datapath state from the
// resolved mappings: DNAT and its exemption where this node accepts, the mark,
// exemption, link, rules and route where this node holds a remote pod, and the
// return-link device on whichever end this node sits. Every slice is sorted at
// the end so the State never depends on map or input order.
func buildState(st *datapath.State, in Inputs, idx *index, mappings []*mapping, alloc map[string]classAlloc) {
	for _, m := range mappings {
		if !m.accepted || m.endpoint == nil || m.effAccepting == "" {
			continue
		}
		emitMapping(st, in, idx, m, alloc[m.pm.Spec.ClassName])
	}
	sortState(st)
}

// emitMapping emits the datapath objects this node owns for one mapping.
func emitMapping(st *datapath.State, in Inputs, idx *index, m *mapping, alloc classAlloc) {
	this := in.NodeName
	endpointNode := m.endpoint.node
	addr, err := netip.ParseAddr(m.endpoint.addr)
	if err != nil {
		return
	}
	sel := portSel(m.pm)

	// Accepting side: DNAT per interface plus the masquerade exemption, on
	// every programmer. Under Single that is one node; under Multi it is every
	// accepting node, so the port answers wherever a routed address lands.
	if contains(m.programmers, this) {
		// Where the request goes next. A pod on this node is translated
		// straight to, as is a remote pod under Single, which reaches it over
		// the CNI's tunnel. Under Multi a remote pod is translated to the far
		// end of this node's own return link instead: the target then sees the
		// request arrive on a device that names which node forwarded it, which
		// is the one thing the CNI's tunnel cannot carry. See
		// docs/spec-decoupled-inbound.md.
		target := addr
		exemptOif := "cilium_host"
		var link *linkParams
		if this != endpointNode {
			if slot, ok := alloc.landedSlotFor(m, this); ok {
				if lp, ok := buildLinkParams(idx, m.class, alloc.subnet, this, endpointNode, slot); ok {
					link = &lp
				}
			}
		}
		if link != nil && servingModeOf(m.class) == v1alpha1.ServingMulti {
			target = link.peerEnd
			exemptOif = link.name
		}

		for _, iface := range interfacesFor(m.class, this, m.pm) {
			st.DNAT = append(st.DNAT, datapath.DNATRule{
				Iface:  iface,
				Port:   sel,
				ToAddr: target,
			})
		}
		dst := target
		st.Exempt = append(st.Exempt, datapath.ExemptRule{
			OifName:   exemptOif,
			Negate:    false,
			DstAddr:   &dst,
			Port:      sel,
			PortIsSrc: false,
		})
		// A remote pod needs the return-link device on this end, and only
		// once the claim has landed: a tentative slot must not half-build a
		// link whose mark could collide.
		if link != nil {
			st.Links = append(st.Links, linkDevice(*link))
		}
	}

	// Target side: this node holds the pod and at least one programmer is
	// elsewhere. Each remote programmer gets its own link, slot and routing
	// table, because a reply has to leave by the link its request arrived on.
	remotePeers := remoteProgrammers(m, this)
	if this == endpointNode && len(remotePeers) > 0 {
		src := addr
		st.Exempt = append(st.Exempt, datapath.ExemptRule{
			OifName:   "cilium_*",
			Negate:    true,
			SrcAddr:   &src,
			Port:      sel,
			PortIsSrc: true,
		})

		multi := servingModeOf(m.class) == v1alpha1.ServingMulti
		if multi {
			// One rule for every peer: the reply carries whichever mark its
			// request stored, and a flow with no entry restores 0 and leaves
			// by this node's own uplink.
			st.CtLoad = append(st.CtLoad, datapath.CtLoadRule{SrcAddr: src, Port: sel})
		}

		for _, peer := range remotePeers {
			// The mark carries the slot, so nothing that depends on the slot is
			// written until the claim has landed: an unresolved slot must never
			// become a real mark, and the divert rules and link that share the
			// slot appear with it, atomically per pass.
			slot, ok := alloc.landedSlotFor(m, peer)
			if !ok {
				continue
			}
			lp, ok := buildLinkParams(idx, m.class, alloc.subnet, this, peer, slot)
			if !ok {
				continue
			}
			if multi {
				// The request arrives on this peer's own link addressed to
				// this node's end of it, which is a local address, so it is
				// delivered here and traverses netfilter. Translating it to
				// the pod is what the accepting node deliberately left undone.
				// Matching the link address as well as the device keeps the
				// rule to kuport's own traffic.
				linkDst := lp.thisEnd
				st.DNAT = append(st.DNAT, datapath.DNATRule{
					Iface:   lp.name,
					Port:    sel,
					ToAddr:  src,
					DstAddr: &linkDst,
				})
				// Which peer forwarded a request is known only as it arrives,
				// so it is written into the flow rather than derived from the
				// packet, which carries nothing that says which.
				st.CtSave = append(st.CtSave, datapath.CtSaveRule{
					Iface: lp.name,
					Mark:  lp.mark,
				})
			} else {
				st.Mark = append(st.Mark, datapath.MarkRule{
					SrcAddr: src,
					Port:    sel,
					Mark:    lp.mark,
				})
			}
			st.Links = append(st.Links, linkDevice(lp))
			st.Rules = append(st.Rules, loopGuardRule(lp), divertRule(lp))
			st.Routes = append(st.Routes, returnRoute(lp))
		}
	}
}

// remoteProgrammers returns, sorted, the mapping's programmers that are not the
// given node. On the pod's node that is every peer needing a return link.
func remoteProgrammers(m *mapping, this string) []string {
	var out []string
	for _, p := range m.programmers {
		if p != this {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// resolveProgrammed settles the Programmed condition for the mappings
// decideRole gated. True requires the participants: the serving node S's
// class node row ready and, when the pod is remote, a landed return-path
// slot whose link both nodes' addresses can build. Sibling accepting nodes
// do not serve the mapping and do not gate its condition. Until the gate
// clears the status names what is missing; the level-driven requeue heals
// it and the reported state is honest during the window.
func resolveProgrammed(in Inputs, idx *index, mappings []*mapping, alloc map[string]classAlloc) {
	for _, m := range mappings {
		if !m.gated {
			continue
		}
		if msg, ok := servingRowReady(m); !ok {
			m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonNodeNotReady, msg, in.Now)
			continue
		}
		// The mapping's interfaces are allowed to name interfaces only some of
		// the class's nodes carry, so the set can come out empty on the node
		// that ended up serving. The port would then answer nowhere, which is
		// worth saying rather than programming nothing in silence.
		if len(interfacesFor(m.class, m.serving, m.pm)) == 0 {
			m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonNoInterfaceOnNode,
				fmt.Sprintf("none of the requested interfaces (%s) are on serving node %s, which class %q gives %s",
					strings.Join(m.pm.Spec.Interfaces, ", "), m.serving, m.class.Name,
					strings.Join(classInterfacesOn(m.class, m.serving), ", ")), in.Now)
			continue
		}
		if m.remote {
			a := alloc[m.pm.Spec.ClassName]
			// Every programmer off the pod's node needs its own landed slot
			// and a buildable link. Under Single there is one; under Multi the
			// mapping is not fully programmed until the last of them is.
			stalled := false
			for _, peer := range remoteProgrammers(m, m.endpoint.node) {
				slot, ok := a.landedSlotFor(m, peer)
				if !ok {
					m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonReturnPathUnavailable,
						fmt.Sprintf("return link for %s is not claimed yet",
							pairKey(peer, m.endpoint.node)), in.Now)
					stalled = true
					break
				}
				if _, ok := buildLinkParams(idx, m.class, a.subnet, peer, m.endpoint.node, slot); !ok {
					m.setProgrammed(metav1.ConditionFalse, v1alpha1.ReasonReturnPathUnavailable,
						fmt.Sprintf("node %s on the return path has no usable address", peer), in.Now)
					stalled = true
					break
				}
			}
			if stalled {
				continue
			}
		}
		m.setProgrammed(metav1.ConditionTrue, v1alpha1.ReasonAllNodesReady, "", in.Now)
	}
}

// servingRowReady reports whether the mapping's serving node has written a
// ready row to the class status. Per the 2026-09-06 adjudication this is
// the whole row gate: the accepting sibling's row is a per-node readiness
// fact, not a condition on the mapping node-a serves. The message names the
// serving node and what its row reported.
func servingRowReady(m *mapping) (string, bool) {
	row := classNodeRow(m.class, m.serving)
	switch {
	case row == nil:
		return fmt.Sprintf("serving node %s has not reported a class status row", m.serving), false
	case !row.Ready:
		return fmt.Sprintf("serving node %s is not ready: %s", m.serving, row.Message), false
	}
	return "", true
}

// portSel builds the datapath PortSel for a mapping's protocol and interval.
func portSel(pm *v1alpha1.PortMap) datapath.PortSel {
	first, last := interval(pm)
	return datapath.PortSel{
		Proto: strings.ToLower(string(pm.Spec.Protocol)),
		First: uint16(first),
		Last:  uint16(last),
	}
}

// linkDevice builds the vxlan device entry for one end of a return link.
func linkDevice(lp linkParams) datapath.Link {
	return datapath.Link{
		Name:       lp.name,
		VNI:        lp.vni,
		Port:       lp.port,
		LocalAddr:  lp.localAddr,
		RemoteAddr: lp.remoteAddr,
		LinkAddr:   lp.linkAddr,
		MTU:        lp.mtu,
	}
}

// loopGuardRule is the pref-101 rule that keeps the encapsulated reply, whose
// mark the vxlan driver copies onto it, from being routed back into the device
// it just left. Without it the kernel drops it silently and bumps tx_errors.
func loopGuardRule(lp linkParams) datapath.IPRule {
	to := lp.peerNode32
	return datapath.IPRule{Pref: 101, Mark: lp.mark, To: &to, Table: 254}
}

// divertRule is the pref-102 rule that sends the marked reply to the return
// table.
func divertRule(lp linkParams) datapath.IPRule {
	return datapath.IPRule{Pref: 102, Mark: lp.mark, To: nil, Table: lp.table}
}

// returnRoute sends the return table's default out the link to the accepting node.
func returnRoute(lp linkParams) datapath.Route {
	return datapath.Route{Table: lp.table, Via: lp.peerEnd, Dev: lp.name}
}

// index holds the lookups Compute needs, built once per pass so nothing rebuilds
// them per mapping.
type index struct {
	nodesByName map[string]*corev1.Node
	nsByName    map[string]*corev1.Namespace
	classByName map[string]*v1alpha1.PortMapClass
	nodeAddr    map[string]string // node name -> InternalIP, "" when unknown
}

func newIndex(in Inputs) *index {
	idx := &index{
		nodesByName: make(map[string]*corev1.Node, len(in.Nodes)),
		nsByName:    make(map[string]*corev1.Namespace, len(in.Namespaces)),
		classByName: make(map[string]*v1alpha1.PortMapClass, len(in.Classes)),
		nodeAddr:    make(map[string]string, len(in.Nodes)),
	}
	for _, n := range in.Nodes {
		idx.nodesByName[n.Name] = n
		idx.nodeAddr[n.Name] = internalIP(n)
	}
	for _, ns := range in.Namespaces {
		idx.nsByName[ns.Name] = ns
	}
	for _, c := range in.Classes {
		idx.classByName[c.Name] = c
	}
	return idx
}

// internalIP returns the node's InternalIP address, or "" when it has none.
func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// sortState orders every slice of a State by a total, deterministic key so the
// output never carries map or input ordering. Determinism here is a correctness
// property: two agents must emit byte-identical state for the same inputs.
func sortState(st *datapath.State) {
	sort.Slice(st.DNAT, func(i, j int) bool { return dnatKey(st.DNAT[i]) < dnatKey(st.DNAT[j]) })
	sort.Slice(st.Exempt, func(i, j int) bool { return exemptKey(st.Exempt[i]) < exemptKey(st.Exempt[j]) })
	sort.Slice(st.Mark, func(i, j int) bool { return markKey(st.Mark[i]) < markKey(st.Mark[j]) })
	sort.Slice(st.Links, func(i, j int) bool { return linkKey(st.Links[i]) < linkKey(st.Links[j]) })
	sort.Slice(st.Rules, func(i, j int) bool { return ruleKey(st.Rules[i]) < ruleKey(st.Rules[j]) })
	sort.Slice(st.Routes, func(i, j int) bool { return routeKey(st.Routes[i]) < routeKey(st.Routes[j]) })
}

func selKey(p datapath.PortSel) string {
	return fmt.Sprintf("%s/%05d/%05d", p.Proto, p.First, p.Last)
}

func dnatKey(r datapath.DNATRule) string {
	return r.Iface + "|" + selKey(r.Port) + "|" + r.ToAddr.String()
}

func exemptKey(r datapath.ExemptRule) string {
	return fmt.Sprintf("%s|%t|%s|%s|%s|%t", r.OifName, r.Negate,
		addrPtrKey(r.DstAddr), addrPtrKey(r.SrcAddr), selKey(r.Port), r.PortIsSrc)
}

func markKey(r datapath.MarkRule) string {
	return fmt.Sprintf("%s|%s|%010d", r.SrcAddr.String(), selKey(r.Port), r.Mark)
}

func linkKey(l datapath.Link) string {
	return l.Name + "|" + l.LinkAddr.String()
}

func ruleKey(r datapath.IPRule) string {
	to := ""
	if r.To != nil {
		to = r.To.String()
	}
	return fmt.Sprintf("%010d|%010d|%s|%010d", r.Pref, r.Mark, to, r.Table)
}

func routeKey(r datapath.Route) string {
	return fmt.Sprintf("%010d|%s|%s", r.Table, r.Via.String(), r.Dev)
}

func addrPtrKey(a *netip.Addr) string {
	if a == nil {
		return "-"
	}
	return a.String()
}

// acceptingNodesFor returns, sorted by name, the nodes a class names that the
// cluster still has. A name in the class with no Node object is left out, so a
// node removed from the cluster stops accepting without the class being edited.
func (idx *index) acceptingNodesFor(class *v1alpha1.PortMapClass) []string {
	var out []string
	for name := range class.Spec.Nodes {
		if _, ok := idx.nodesByName[name]; ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
