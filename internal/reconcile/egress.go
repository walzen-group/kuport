package reconcile

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// The pod's node routes a selected pod's marked flow after every other policy
// rule on the node: Cilium's at 9, netbird's at 105 and 110. The suppress rule
// lets main answer with any route more specific than its default, so a netbird
// peer, the LAN or a pod CIDR keeps its path, and only what would take main's
// default route reaches the link's table at egressDivertPref. The loop guard
// keeps the VXLAN outer packet, which carries the same mark, out of the link.
const (
	egressLoopGuardPref = 32763
	egressSuppressPref  = 32764
	egressDivertPref    = 32765
	mainTable           = 254
)

// egressDecision is one Egress resolved: whether it is accepted, its class's
// egress node, and the pods it decides the path of.
type egressDecision struct {
	eg       *v1alpha1.Egress
	class    *v1alpha1.EgressClass
	accepted metav1.Condition
	ok       bool
	node     string // the egress node, "" when the class names no Node
	pods     []*egressPod
	// conflicts names the selected pods an earlier Egress decides.
	conflicts []string
}

// egressPod is one selected pod and its path in each family.
type egressPod struct {
	name, node  string
	addr, addr6 netip.Addr
	path, path6 v1alpha1.EgressPath
	slot        int
	linked      bool
	has6        bool
	failClosed  bool
	// underlay reads a node's underlay MTU from the EgressClass status.
	underlay func(string) int
}

// egressClassKey is the link class of an EgressClass.
func egressClassKey(name string) linkClass {
	return linkClass{kind: v1alpha1.ClassKindEgressClass, name: name}
}

// resolveEgresses decides every Egress: validation, then pod selection in
// creation order, so a pod two Egresses select belongs to the earlier one, the
// order conflicts.go uses for ports.
func resolveEgresses(in Inputs, idx *index) []*egressDecision {
	classes := map[string]*v1alpha1.EgressClass{}
	for _, c := range in.EgressClasses {
		classes[c.Name] = c
	}

	egs := append([]*v1alpha1.Egress(nil), in.Egresses...)
	sort.Slice(egs, func(i, j int) bool {
		a, b := egs[i], egs[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})

	claimed := map[string]bool{}
	var out []*egressDecision
	for _, eg := range egs {
		d := &egressDecision{eg: eg, class: classes[eg.Spec.ClassName]}
		reason, msg := v1alpha1.ReasonValid, ""
		switch {
		case d.class == nil:
			reason, msg = v1alpha1.ReasonClassNotFound, fmt.Sprintf("EgressClass %q does not exist", eg.Spec.ClassName)
		case !selectorSelectsNamespace(idx, d.class.Spec.NamespaceSelector, eg.Namespace):
			reason, msg = v1alpha1.ReasonNamespaceNotSelected, fmt.Sprintf("EgressClass %q does not select namespace %q", d.class.Name, eg.Namespace)
		}
		d.ok = reason == v1alpha1.ReasonValid
		status := metav1.ConditionTrue
		if !d.ok {
			status = metav1.ConditionFalse
		}
		d.accepted = metav1.Condition{
			Type: v1alpha1.ConditionAccepted, Status: status, Reason: reason, Message: msg,
			LastTransitionTime: in.Now, ObservedGeneration: eg.Generation,
		}
		if d.ok {
			if _, exists := idx.nodesByName[d.class.Spec.Node]; exists {
				d.node = d.class.Spec.Node
			}
			for _, p := range selectedPods(in, eg) {
				key := p.Namespace + "/" + p.Name
				if claimed[key] {
					d.conflicts = append(d.conflicts, p.Name)
					continue
				}
				claimed[key] = true
				d.pods = append(d.pods, newEgressPod(p))
			}
		}
		out = append(out, d)
	}
	return out
}

// selectedPods returns the pods an Egress's selector picks in its namespace that
// are scheduled, hold an address and have not finished, sorted by name.
func selectedPods(in Inputs, eg *v1alpha1.Egress) []*corev1.Pod {
	sel, err := metav1.LabelSelectorAsSelector(&eg.Spec.PodSelector)
	if err != nil || sel.Empty() {
		return nil
	}
	var out []*corev1.Pod
	for _, p := range in.Pods {
		if p.Namespace != eg.Namespace || p.Spec.NodeName == "" || len(p.Status.PodIPs) == 0 {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if sel.Matches(labels.Set(p.Labels)) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func newEgressPod(p *corev1.Pod) *egressPod {
	ep := &egressPod{name: p.Name, node: p.Spec.NodeName}
	for _, ip := range p.Status.PodIPs {
		a, err := netip.ParseAddr(ip.IP)
		if err != nil {
			continue
		}
		switch {
		case a.Is4() && !ep.addr.IsValid():
			ep.addr = a
		case a.Is6() && !a.Is4In6() && !ep.addr6.IsValid():
			ep.addr6 = a
		}
	}
	return ep
}

// addEgressNeeds adds the link pairs the Egresses need: one from each selected
// pod's node to the egress node, proposed by the pod's node.
func addEgressNeeds(needed map[string]neededPair, decisions []*egressDecision) {
	for _, d := range decisions {
		if !d.ok || d.node == "" {
			continue
		}
		for _, p := range d.pods {
			if p.node != d.node && p.addr.IsValid() {
				addNeeded(needed, egressClassKey(d.class.Name), d.node, p.node)
			}
		}
	}
}

// decidePaths settles every pod's path from inputs all agents share, so the
// pod's node programs what the egress node reports.
func decidePaths(in Inputs, idx *index, decisions []*egressDecision, alloc linkAlloc) {
	cidrsKnown := podCIDRsKnown(in)
	for _, d := range decisions {
		if !d.ok {
			continue
		}
		underlay := egressClassUnderlay(d.class)
		for _, p := range d.pods {
			p.failClosed = d.eg.Spec.FailClosed
			p.underlay = underlay
			down := v1alpha1.EgressPathDirect
			if p.failClosed {
				down = v1alpha1.EgressPathBlocked
			}
			p.path, p.path6 = down, ""
			if p.addr6.IsValid() {
				p.path6 = down
			}

			switch {
			case d.node != "" && p.node == d.node:
				p.path = v1alpha1.EgressPathLocal
				if p.addr6.IsValid() {
					p.path6 = v1alpha1.EgressPathLocal
				}
				continue
			case d.node == "" || !cidrsKnown || !p.addr.IsValid():
				continue
			}

			// A powered-off node keeps its class row, so its Node's Ready
			// condition is what takes the path down.
			if !nodeReady(idx, d.node) || !nodeReady(idx, p.node) {
				continue
			}
			slot, ok := alloc.slotFor(egressClassKey(d.class.Name), d.node, p.node)
			if !ok || !egressRowReady(d.class, d.node) || !egressRowReady(d.class, p.node) {
				continue
			}
			lp, ok := buildLinkParams(idx, underlay, p.node, d.node, slot, linkPort(in))
			if !ok {
				continue
			}
			p.slot, p.linked, p.path = slot, true, v1alpha1.EgressPathLink
			if p.addr6.IsValid() && lp.has6 && egressRow6(d.class, d.node) && egressRow6(d.class, p.node) {
				p.has6, p.path6 = true, v1alpha1.EgressPathLink
			}
		}
	}
}

// emitEgress adds this node's egress objects to the State: the pod's node's
// marks, links and routing, and the egress node's gateway half.
func emitEgress(st *datapath.State, in Inputs, idx *index, decisions []*egressDecision) {
	blockedFams := map[datapath.Family]bool{}
	for _, d := range decisions {
		if !d.ok {
			continue
		}
		for _, p := range d.pods {
			if p.path == v1alpha1.EgressPathLocal {
				continue
			}
			if p.node == in.NodeName {
				emitEgressPodNode(st, in, idx, d, p, blockedFams)
			}
			if d.node == in.NodeName && p.linked {
				emitEgressGateway(st, in, idx, d, p)
			}
		}
	}
	emitBlocked(st, blockedFams)
	if len(st.Egress) > 0 {
		st.ClusterCIDRs = clusterCIDRs(in)
	}
}

func emitEgressPodNode(st *datapath.State, in Inputs, idx *index, d *egressDecision, p *egressPod, blockedFams map[datapath.Family]bool) {
	type fam struct {
		addr netip.Addr
		path v1alpha1.EgressPath
		f    datapath.Family
	}
	var fams []fam
	if p.addr.IsValid() {
		fams = append(fams, fam{p.addr, p.path, datapath.FamilyIPv4})
	}
	if p.addr6.IsValid() && in.IPv6Forwarding {
		fams = append(fams, fam{p.addr6, p.path6, datapath.FamilyIPv6})
	}

	var lp linkParams
	if p.linked {
		lp, _ = buildLinkParams(idx, p.underlay, p.node, d.node, p.slot, linkPort(in))
		st.Links = append(st.Links, linkDevice(lp, p.has6 && in.IPv6Forwarding))
		st.Rules = append(st.Rules, datapath.IPRule{Pref: egressLoopGuardPref, Mark: lp.mark, To: &lp.peerNode32, Table: mainTable})
	}
	for _, f := range fams {
		switch f.path {
		case v1alpha1.EgressPathLink:
			addr := f.addr
			st.Egress = append(st.Egress, datapath.EgressRule{SrcAddr: addr, Mark: lp.mark})
			st.Exempt = append(st.Exempt, datapath.ExemptRule{OifName: lp.name, SrcAddr: &addr})
			st.Rules = append(st.Rules,
				datapath.IPRule{Pref: egressSuppressPref, Mark: lp.mark, Table: mainTable, SuppressDefault: true, Family: f.f},
				datapath.IPRule{Pref: egressDivertPref, Mark: lp.mark, Table: lp.table, Family: f.f},
			)
			st.Routes = append(st.Routes, returnRoute(lp, f.f))
		case v1alpha1.EgressPathBlocked:
			st.Egress = append(st.Egress, datapath.EgressRule{SrcAddr: f.addr, Mark: datapath.EgressBlockedMark})
			blockedFams[f.f] = true
		}
	}
}

// emitBlocked adds, once per family, the routing that sends a blocked pod's
// default-route traffic into the blackhole while specific routes still answer.
func emitBlocked(st *datapath.State, fams map[datapath.Family]bool) {
	for _, f := range []datapath.Family{datapath.FamilyIPv4, datapath.FamilyIPv6} {
		if !fams[f] {
			continue
		}
		st.Rules = append(st.Rules,
			datapath.IPRule{Pref: egressSuppressPref, Mark: datapath.EgressBlockedMark, Table: mainTable, SuppressDefault: true, Family: f},
			datapath.IPRule{Pref: egressDivertPref, Mark: datapath.EgressBlockedMark, Table: datapath.EgressBlockedTable, Family: f},
		)
		st.Routes = append(st.Routes, datapath.Route{Table: datapath.EgressBlockedTable, Blackhole: true, BlackholeFamily: f})
	}
}

// emitEgressGateway is the egress node's half of one pod's link. The reply has
// to go back into the link although the CNI routes the pod's address more
// specifically, so it diverts at the prefs a PortMap's return path uses.
func emitEgressGateway(st *datapath.State, in Inputs, idx *index, d *egressDecision, p *egressPod) {
	lp, ok := buildLinkParams(idx, p.underlay, d.node, p.node, p.slot, linkPort(in))
	if !ok {
		return
	}
	v6 := p.has6 && in.IPv6Forwarding
	st.Links = append(st.Links, linkDevice(lp, v6))
	st.EgressGateway = append(st.EgressGateway, datapath.EgressGatewayRule{Iface: lp.name, Mark: lp.mark})
	st.Rules = append(st.Rules, loopGuardRule(lp), divertRule(lp, datapath.FamilyIPv4))
	st.Routes = append(st.Routes, returnRoute(lp, datapath.FamilyIPv4))
	if v6 {
		st.Rules = append(st.Rules, divertRule(lp, datapath.FamilyIPv6))
		st.Routes = append(st.Routes, returnRoute(lp, datapath.FamilyIPv6))
	}
}

// keepState is what the agent leaves on its node when it stops: every
// fail-closed pod here held in the blackhole, so the next agent's first pass
// takes over with no window where the pod's default-route traffic leaves from
// this node.
func keepState(in Inputs, decisions []*egressDecision) datapath.State {
	var st datapath.State
	fams := map[datapath.Family]bool{}
	for _, d := range decisions {
		if !d.ok {
			continue
		}
		for _, p := range d.pods {
			if p.node != in.NodeName || !p.failClosed || p.path == v1alpha1.EgressPathLocal {
				continue
			}
			if p.addr.IsValid() {
				st.Egress = append(st.Egress, datapath.EgressRule{SrcAddr: p.addr, Mark: datapath.EgressBlockedMark})
				fams[datapath.FamilyIPv4] = true
			}
			if p.addr6.IsValid() && in.IPv6Forwarding {
				st.Egress = append(st.Egress, datapath.EgressRule{SrcAddr: p.addr6, Mark: datapath.EgressBlockedMark})
				fams[datapath.FamilyIPv6] = true
			}
		}
	}
	if len(st.Egress) == 0 {
		return datapath.State{}
	}
	emitBlocked(&st, fams)
	st.ClusterCIDRs = clusterCIDRs(in)
	sortState(&st)
	return st
}

// clusterCIDRs are the destinations an egress rule leaves alone: every Node's
// pod CIDRs and InternalIPs.
func clusterCIDRs(in Inputs) []netip.Prefix {
	var out []netip.Prefix
	for _, n := range in.Nodes {
		for _, c := range n.Spec.PodCIDRs {
			if p, err := netip.ParsePrefix(c); err == nil {
				out = append(out, p.Masked())
			}
		}
		for _, a := range n.Status.Addresses {
			if a.Type != corev1.NodeInternalIP {
				continue
			}
			if ip, err := netip.ParseAddr(a.Address); err == nil {
				out = append(out, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
			}
		}
	}
	return out
}

// podCIDRsKnown reports whether every Node names its pod CIDRs. Without them
// the egress rule cannot leave the cluster's own pods alone.
func podCIDRsKnown(in Inputs) bool {
	for _, n := range in.Nodes {
		if len(n.Spec.PodCIDRs) == 0 {
			return false
		}
	}
	return len(in.Nodes) > 0
}

// nodeReady reports a Node's Ready condition. A Node reporting no Ready
// condition counts as ready; one whose condition is False or Unknown, which the
// node controller sets about 40 seconds after the kubelet stops reporting, does
// not.
func nodeReady(idx *index, name string) bool {
	n := idx.nodesByName[name]
	if n == nil {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return true
}

func egressClassRow(class *v1alpha1.EgressClass, name string) *v1alpha1.NodeStatus {
	for i := range class.Status.Nodes {
		if class.Status.Nodes[i].Name == name {
			return &class.Status.Nodes[i]
		}
	}
	return nil
}

func egressRowReady(class *v1alpha1.EgressClass, name string) bool {
	r := egressClassRow(class, name)
	return r != nil && r.Ready
}

func egressRow6(class *v1alpha1.EgressClass, name string) bool {
	r := egressClassRow(class, name)
	return r != nil && r.IPv6Unavailable == ""
}

func egressClassUnderlay(class *v1alpha1.EgressClass) func(string) int {
	return func(name string) int {
		if r := egressClassRow(class, name); r != nil {
			return int(r.UnderlayMTU)
		}
		return 0
	}
}

// EgressClassContribution is one node's row and the class conditions.
type EgressClassContribution struct {
	Node       v1alpha1.NodeStatus
	Conditions []metav1.Condition
}

// buildEgressClassStatus gives a row to every class this node takes part in:
// as its egress node, or as the node of a pod one of its Egresses selects.
func buildEgressClassStatus(res *Result, in Inputs, decisions []*egressDecision, alloc linkAlloc) {
	takesPart := map[string]bool{}
	for _, c := range in.EgressClasses {
		if c.Spec.Node == in.NodeName {
			takesPart[c.Name] = true
		}
	}
	for _, d := range decisions {
		if !d.ok {
			continue
		}
		for _, p := range d.pods {
			if p.node == in.NodeName {
				takesPart[d.class.Name] = true
			}
		}
	}
	for _, c := range in.EgressClasses {
		if !takesPart[c.Name] {
			continue
		}
		row := v1alpha1.NodeStatus{Name: in.NodeName, Ready: true}
		if !in.IPv6Forwarding {
			row.IPv6Unavailable = ipv6ForwardingOff
		}
		ready := metav1.Condition{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonValid,
			LastTransitionTime: in.Now, ObservedGeneration: c.Generation,
		}
		if alloc.exhausted[egressClassKey(c.Name)] {
			ready.Status, ready.Reason = metav1.ConditionFalse, v1alpha1.ReasonSubnetExhausted
			ready.Message = fmt.Sprintf("all %d slots of the cluster-wide link pool are taken", linkSlots)
		}
		res.EgressClassStatus[c.Name] = EgressClassContribution{Node: row, Conditions: []metav1.Condition{ready}}
	}
}

// buildEgressStatus writes the status of every Egress this node owns: the one
// whose class names this node, or, for an Egress with no egress node, the
// first node of the cluster by name.
func buildEgressStatus(res *Result, in Inputs, decisions []*egressDecision, alloc linkAlloc) {
	first := ""
	for _, n := range in.Nodes {
		if first == "" || n.Name < first {
			first = n.Name
		}
	}
	cidrsKnown := podCIDRsKnown(in)
	for _, d := range decisions {
		owner := d.node
		if owner == "" {
			owner = first
		}
		if owner != in.NodeName {
			continue
		}
		st := v1alpha1.EgressStatus{Node: d.node, ObservedGeneration: d.eg.Generation}
		for _, p := range d.pods {
			ep := v1alpha1.EgressPod{Name: p.name, Node: p.node, Path: p.path}
			if p.addr.IsValid() {
				ep.Address = p.addr.String()
			}
			if p.addr6.IsValid() {
				ep.Address6, ep.Path6 = p.addr6.String(), p.path6
			}
			st.Pods = append(st.Pods, ep)
		}
		st.Conditions = []metav1.Condition{d.accepted}
		if d.ok {
			st.Conditions = append(st.Conditions, egressProgrammed(in, d, alloc, cidrsKnown))
		}
		sortConditions(st.Conditions)
		res.EgressStatus[types.NamespacedName{Namespace: d.eg.Namespace, Name: d.eg.Name}] = st
	}
}

func egressProgrammed(in Inputs, d *egressDecision, alloc linkAlloc, cidrsKnown bool) metav1.Condition {
	c := metav1.Condition{
		Type: v1alpha1.ConditionProgrammed, Status: metav1.ConditionFalse,
		LastTransitionTime: in.Now, ObservedGeneration: d.eg.Generation,
	}
	var down []string
	for _, p := range d.pods {
		if p.path != v1alpha1.EgressPathLink && p.path != v1alpha1.EgressPathLocal {
			down = append(down, p.name)
		}
	}
	switch {
	case d.node == "":
		c.Reason = v1alpha1.ReasonEgressNodeNotFound
		c.Message = fmt.Sprintf("EgressClass %q names node %q, which does not exist", d.class.Name, d.class.Spec.Node)
	case len(d.conflicts) > 0:
		c.Reason = v1alpha1.ReasonEgressConflict
		c.Message = "an earlier Egress selects " + strings.Join(d.conflicts, ", ")
	case !cidrsKnown:
		c.Reason = v1alpha1.ReasonPodCIDRsUnknown
		c.Message = "a Node has no spec.podCIDRs, so the cluster's own destinations cannot be left alone"
	case alloc.exhausted[egressClassKey(d.class.Name)]:
		c.Reason = v1alpha1.ReasonSubnetExhausted
		c.Message = fmt.Sprintf("all %d slots of the cluster-wide link pool are taken", linkSlots)
	case len(down) > 0:
		c.Reason = v1alpha1.ReasonReturnPathUnavailable
		c.Message = "the path to " + d.node + " is down for " + strings.Join(down, ", ") +
			": no Link yet, a node not ready, or a class row missing or not ready"
	default:
		c.Status, c.Reason = metav1.ConditionTrue, v1alpha1.ReasonAllPodsRouted
	}
	return c
}

// selectorSelectsNamespace applies a class's namespace selector. A nil or empty
// selector selects every namespace.
func selectorSelectsNamespace(idx *index, s *metav1.LabelSelector, namespace string) bool {
	if s == nil {
		return true
	}
	sel, err := metav1.LabelSelectorAsSelector(s)
	if err != nil {
		return false
	}
	if sel.Empty() {
		return true
	}
	ns := idx.nsByName[namespace]
	return ns != nil && sel.Matches(labels.Set(ns.Labels))
}
