package reconcile

import (
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// egressWorld: node-a is the egress node, the selected pod qb-0 runs on node-b,
// node-c holds nothing. The class reports both rows ready and slot-3 holds the
// link, so the path is Link unless a test takes something away.
func egressWorld(runOn string, failClosed bool) Inputs {
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			podCIDRNode("node-a", "10.0.0.1", "10.244.0.0/24", "fd00:10:244::/64"),
			podCIDRNode("node-b", "10.0.0.2", "10.244.5.0/24", "fd00:10:244:5::/64"),
			podCIDRNode("node-c", "10.0.0.3", "10.244.9.0/24"),
		},
		Namespaces:    []*corev1.Namespace{ns("media", nil)},
		EgressClasses: []*v1alpha1.EgressClass{egressClass("out", "node-a", readyRows("node-a", "node-b"))},
		Egresses:      []*v1alpha1.Egress{egress("media", "qb", "out", failClosed, 0)},
		Pods:          []*corev1.Pod{podOn("media", "qb-0", "node-b", "10.244.5.5")},
		Links:         []*v1alpha1.Link{linkOf(v1alpha1.ClassKindEgressClass, "out", "node-a", "node-b", 3)},
		Now:           metav1.NewTime(baseTime),
	}
}

func podCIDRNode(name, ip string, cidrs ...string) *corev1.Node {
	n := node(name, ip, nil)
	n.Spec.PodCIDRs = cidrs
	return n
}

func readyRows(names ...string) []v1alpha1.NodeStatus {
	var out []v1alpha1.NodeStatus
	for _, n := range names {
		out = append(out, v1alpha1.NodeStatus{Name: n, Ready: true})
	}
	return out
}

func egressClass(name, node string, rows []v1alpha1.NodeStatus) *v1alpha1.EgressClass {
	return &v1alpha1.EgressClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec:       v1alpha1.EgressClassSpec{Node: node},
		Status:     v1alpha1.EgressClassStatus{Nodes: rows},
	}
}

func egress(namespace, name, className string, failClosed bool, createdOffset int) *v1alpha1.Egress {
	return &v1alpha1.Egress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, Generation: 1,
			CreationTimestamp: metav1.NewTime(baseTime.Add(time.Duration(createdOffset) * time.Second)),
		},
		Spec: v1alpha1.EgressSpec{
			ClassName:   className,
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "qb"}},
			FailClosed:  failClosed,
		},
	}
}

func podOn(namespace, name, node string, ips ...string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{"app": "qb"}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, ip := range ips {
		p.Status.PodIPs = append(p.Status.PodIPs, corev1.PodIP{IP: ip})
	}
	return p
}

func egressStatusOf(res Result, namespace, name string) (v1alpha1.EgressStatus, bool) {
	st, ok := res.EgressStatus[types.NamespacedName{Namespace: namespace, Name: name}]
	return st, ok
}

func TestEgressPodNodeRoutesIntoTheLink(t *testing.T) {
	st := Compute(egressWorld("node-b", true)).State
	pod := netip.MustParseAddr("10.244.5.5")
	mark := linkMark(3)

	if want := []datapath.EgressRule{{SrcAddr: pod, Mark: mark}}; !reflect.DeepEqual(st.Egress, want) {
		t.Errorf("egress rules = %+v, want %+v", st.Egress, want)
	}
	if len(st.Exempt) != 1 || st.Exempt[0].OifName != "kup-3" || st.Exempt[0].SrcAddr == nil || *st.Exempt[0].SrcAddr != pod {
		t.Errorf("exempt = %+v, want oifname kup-3 saddr %s", st.Exempt, pod)
	}
	// After netbird's 105 and 110 and before main at 32766: a specific route
	// in main answers first, and only default-route traffic reaches the link.
	wantRules := []datapath.IPRule{
		{Pref: 32763, Mark: mark, To: prefixPtr("10.0.0.1/32"), Table: 254},
		{Pref: 32764, Mark: mark, Table: 254, SuppressDefault: true},
		{Pref: 32765, Mark: mark, Table: 10003},
	}
	if fmt.Sprint(st.Rules) != fmt.Sprint(wantRules) {
		t.Errorf("rules =\n%v\nwant\n%v", st.Rules, wantRules)
	}
	wantRoutes := []datapath.Route{{Table: 10003, Via: netip.MustParseAddr("169.254.64.6"), Dev: "kup-3"}}
	if !reflect.DeepEqual(st.Routes, wantRoutes) {
		t.Errorf("routes = %+v, want %+v", st.Routes, wantRoutes)
	}
	if len(st.Links) != 1 || st.Links[0].Name != "kup-3" || st.Links[0].LinkAddr.String() != "169.254.64.7/31" {
		t.Errorf("links = %+v, want kup-3 at 169.254.64.7/31", st.Links)
	}
	for _, c := range []string{"10.244.0.0/24", "10.244.5.0/24", "10.244.9.0/24", "10.0.0.1/32", "fd00:10:244::/64"} {
		found := false
		for _, p := range st.ClusterCIDRs {
			found = found || p.String() == c
		}
		if !found {
			t.Errorf("cluster CIDRs %v lack %s", st.ClusterCIDRs, c)
		}
	}
}

func TestEgressNodeMasqueradesAndRoutesRepliesBack(t *testing.T) {
	st := Compute(egressWorld("node-a", true)).State
	mark := linkMark(3)
	if want := []datapath.EgressGatewayRule{{Iface: "kup-3", Mark: mark}}; !reflect.DeepEqual(st.EgressGateway, want) {
		t.Errorf("gateway = %+v, want %+v", st.EgressGateway, want)
	}
	// The reply has to go into the link even though the CNI routes the pod's
	// address more specifically, so the egress node diverts at 101/102.
	wantRules := []datapath.IPRule{
		{Pref: 101, Mark: mark, To: prefixPtr("10.0.0.2/32"), Table: 254},
		{Pref: 102, Mark: mark, Table: 10003},
	}
	if fmt.Sprint(st.Rules) != fmt.Sprint(wantRules) {
		t.Errorf("rules =\n%v\nwant\n%v", st.Rules, wantRules)
	}
	wantRoutes := []datapath.Route{{Table: 10003, Via: netip.MustParseAddr("169.254.64.7"), Dev: "kup-3"}}
	if !reflect.DeepEqual(st.Routes, wantRoutes) {
		t.Errorf("routes = %+v, want %+v", st.Routes, wantRoutes)
	}
	if len(st.Egress) != 0 {
		t.Errorf("egress node marks %+v; it holds no selected pod", st.Egress)
	}
}

func TestEgressBystanderEmitsNothing(t *testing.T) {
	res := Compute(egressWorld("node-c", true))
	if !reflect.DeepEqual(res.State, datapath.State{}) {
		t.Errorf("node-c state = %+v, want empty", res.State)
	}
}

func TestEgressPathDownWithoutALink(t *testing.T) {
	blocked := func(in Inputs) Inputs { in.Links = nil; return in }

	t.Run("fail closed blackholes default-route traffic", func(t *testing.T) {
		res := Compute(blocked(egressWorld("node-b", true)))
		st := res.State
		pod := netip.MustParseAddr("10.244.5.5")
		if want := []datapath.EgressRule{{SrcAddr: pod, Mark: datapath.EgressBlockedMark}}; !reflect.DeepEqual(st.Egress, want) {
			t.Errorf("egress rules = %+v, want %+v", st.Egress, want)
		}
		wantRules := []datapath.IPRule{
			{Pref: 32764, Mark: datapath.EgressBlockedMark, Table: 254, SuppressDefault: true},
			{Pref: 32765, Mark: datapath.EgressBlockedMark, Table: datapath.EgressBlockedTable},
		}
		if fmt.Sprint(st.Rules) != fmt.Sprint(wantRules) {
			t.Errorf("rules =\n%v\nwant\n%v", st.Rules, wantRules)
		}
		if want := []datapath.Route{{Table: datapath.EgressBlockedTable, Blackhole: true}}; !reflect.DeepEqual(st.Routes, want) {
			t.Errorf("routes = %+v, want %+v", st.Routes, want)
		}
		if len(st.Links) != 0 {
			t.Errorf("links = %+v, want none before the Link exists", st.Links)
		}
		if c, ok := createFor(res, v1alpha1.ClassKindEgressClass, "out", "node-a", "node-b"); !ok || c.Name != "slot-0" {
			t.Errorf("node-b creates %+v ok=%v, want slot-0 for EgressClass out", c, ok)
		}
	})

	t.Run("fail open leaves the pod alone", func(t *testing.T) {
		st := Compute(blocked(egressWorld("node-b", false))).State
		if len(st.Egress) != 0 || len(st.Rules) != 0 || len(st.Routes) != 0 {
			t.Errorf("state = %+v, want no egress objects while Direct", st)
		}
	})

	t.Run("status reports the path and the reason", func(t *testing.T) {
		for _, fc := range []bool{true, false} {
			st, ok := egressStatusOf(Compute(blocked(egressWorld("node-a", fc))), "media", "qb")
			if !ok {
				t.Fatal("node-a writes no status")
			}
			want := v1alpha1.EgressPathDirect
			if fc {
				want = v1alpha1.EgressPathBlocked
			}
			if len(st.Pods) != 1 || st.Pods[0].Path != want {
				t.Errorf("failClosed=%t: pods = %+v, want path %s", fc, st.Pods, want)
			}
			c := findCond(st.Conditions, v1alpha1.ConditionProgrammed)
			if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonReturnPathUnavailable {
				t.Errorf("failClosed=%t: Programmed = %s/%s, want False/ReturnPathUnavailable", fc, c.Status, c.Reason)
			}
		}
	})
}

func TestEgressPathDownWhileARowIsNotReady(t *testing.T) {
	in := egressWorld("node-b", true)
	in.EgressClasses = []*v1alpha1.EgressClass{egressClass("out", "node-a", []v1alpha1.NodeStatus{
		{Name: "node-a", Ready: false, Message: "agent starting"},
		{Name: "node-b", Ready: true},
	})}
	st := Compute(in).State
	if len(st.Egress) != 1 || st.Egress[0].Mark != datapath.EgressBlockedMark {
		t.Errorf("egress rules = %+v, want the blocked mark", st.Egress)
	}
}

// TestEgressIPv6OnlyPodEmitsNoIPv4Rule: a pod with no IPv4 address gets no
// IPv4 rule, here or in the state kept at shutdown; an invalid source address
// fails the render.
func TestEgressIPv6OnlyPodEmitsNoIPv4Rule(t *testing.T) {
	in := egressWorld("node-b", true)
	in.IPv6Forwarding = true
	in.Pods = []*corev1.Pod{podOn("media", "qb-0", "node-b", "fd00:10:244:5::5")}
	res := Compute(in)
	for _, st := range []datapath.State{res.State, res.Keep} {
		for _, e := range st.Egress {
			if !e.SrcAddr.IsValid() {
				t.Errorf("egress rule with an invalid source: %+v", st.Egress)
			}
		}
	}
}

// TestEgressNodeNotReadyTakesThePathDown: a powered-off egress node keeps its
// class row, so the Node's Ready condition is what tells the pod's node to stop
// sending into the link: Direct under failClosed false, Blocked under true.
func TestEgressNodeNotReadyTakesThePathDown(t *testing.T) {
	for _, fc := range []bool{false, true} {
		in := egressWorld("node-b", fc)
		in.Nodes[0].Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}
		st := Compute(in).State
		switch {
		case !fc && len(st.Egress) != 0:
			t.Errorf("failClosed=false: egress = %+v, want none while node-a is not ready", st.Egress)
		case fc && (len(st.Egress) != 1 || st.Egress[0].Mark != datapath.EgressBlockedMark):
			t.Errorf("failClosed=true: egress = %+v, want the blocked mark while node-a is not ready", st.Egress)
		}
	}
	// A Node reporting no Ready condition at all, as the fixtures do, counts as
	// ready, so a cluster whose status lacks it keeps working.
	if st := Compute(egressWorld("node-b", true)).State; len(st.Egress) != 1 || st.Egress[0].Mark != linkMark(3) {
		t.Errorf("egress = %+v, want the link's mark with no Ready condition reported", st.Egress)
	}
}

func TestEgressPodOnTheEgressNodeIsLocal(t *testing.T) {
	in := egressWorld("node-a", true)
	in.Pods = []*corev1.Pod{podOn("media", "qb-0", "node-a", "10.244.0.7")}
	res := Compute(in)
	if !reflect.DeepEqual(res.State, datapath.State{}) {
		t.Errorf("state = %+v, want none: Cilium's masquerade already writes node-a's address", res.State)
	}
	st, _ := egressStatusOf(res, "media", "qb")
	if len(st.Pods) != 1 || st.Pods[0].Path != v1alpha1.EgressPathLocal {
		t.Errorf("pods = %+v, want path Local", st.Pods)
	}
	if c := findCond(st.Conditions, v1alpha1.ConditionProgrammed); c.Status != metav1.ConditionTrue {
		t.Errorf("Programmed = %s/%s, want True", c.Status, c.Reason)
	}
}

func TestEgressDualStack(t *testing.T) {
	in := egressWorld("node-b", true)
	in.IPv6Forwarding = true
	in.Pods = []*corev1.Pod{podOn("media", "qb-0", "node-b", "10.244.5.5", "fd00:10:244:5::5")}
	st := Compute(in).State
	pod6 := netip.MustParseAddr("fd00:10:244:5::5")
	found := false
	for _, e := range st.Egress {
		found = found || (e.SrcAddr == pod6 && e.Mark == linkMark(3))
	}
	if !found {
		t.Errorf("egress rules = %+v, want the pod's IPv6 address marked into kup-3", st.Egress)
	}
	var v6Routes int
	for _, r := range st.Routes {
		if r.Via.Is6() {
			v6Routes++
		}
	}
	if v6Routes != 1 {
		t.Errorf("routes = %+v, want one IPv6 route via the link", st.Routes)
	}
	if len(st.Links) != 1 || !st.Links[0].LinkAddr6.IsValid() {
		t.Errorf("links = %+v, want the link carrying a /127", st.Links)
	}
}

func TestEgressConflictEarlierWins(t *testing.T) {
	in := egressWorld("node-a", true)
	in.EgressClasses = append(in.EgressClasses, egressClass("other", "node-c", readyRows("node-c", "node-b")))
	in.Egresses = append(in.Egresses, egress("media", "late", "other", true, 10))
	res := Compute(in)

	late := findCond(Compute(func() Inputs { i := in; i.NodeName = "node-c"; return i }()).EgressStatus[types.NamespacedName{Namespace: "media", Name: "late"}].Conditions, v1alpha1.ConditionProgrammed)
	if late.Status != metav1.ConditionFalse || late.Reason != v1alpha1.ReasonEgressConflict {
		t.Errorf("late Programmed = %s/%s, want False/EgressConflict", late.Status, late.Reason)
	}
	st, _ := egressStatusOf(res, "media", "qb")
	if len(st.Pods) != 1 {
		t.Errorf("earlier Egress pods = %+v, want qb-0", st.Pods)
	}
	b := Compute(func() Inputs { i := in; i.NodeName = "node-b"; return i }()).State
	if len(b.Egress) != 1 || b.Egress[0].Mark != linkMark(3) {
		t.Errorf("node-b egress = %+v, want the earlier Egress's link", b.Egress)
	}
}

func TestEgressNamespaceNotSelected(t *testing.T) {
	in := egressWorld("node-a", true)
	in.EgressClasses[0].Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"egress": "yes"}}
	res := Compute(in)
	st, _ := egressStatusOf(res, "media", "qb")
	if c := findCond(st.Conditions, v1alpha1.ConditionAccepted); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonNamespaceNotSelected {
		t.Errorf("Accepted = %s/%s, want False/NamespaceNotSelected", c.Status, c.Reason)
	}
	b := egressWorld("node-b", true)
	b.EgressClasses = in.EgressClasses
	if s := Compute(b).State; len(s.Egress) != 0 {
		t.Errorf("node-b egress = %+v, want none for a refused Egress", s.Egress)
	}
}

func TestEgressPodCIDRsUnknown(t *testing.T) {
	in := egressWorld("node-b", true)
	in.Nodes[2].Spec.PodCIDRs = nil
	if st := Compute(in).State; len(st.Egress) != 1 || st.Egress[0].Mark != datapath.EgressBlockedMark {
		t.Errorf("egress = %+v, want blocked while a node's pod CIDRs are unknown", st.Egress)
	}
	in.NodeName = "node-a"
	st, _ := egressStatusOf(Compute(in), "media", "qb")
	if c := findCond(st.Conditions, v1alpha1.ConditionProgrammed); c.Reason != v1alpha1.ReasonPodCIDRsUnknown {
		t.Errorf("Programmed reason = %s, want PodCIDRsUnknown", c.Reason)
	}
}

func TestEgressStatusWrittenByTheEgressNodeOnly(t *testing.T) {
	if _, ok := egressStatusOf(Compute(egressWorld("node-b", true)), "media", "qb"); ok {
		t.Error("node-b writes the Egress status; only the egress node does")
	}
	st, ok := egressStatusOf(Compute(egressWorld("node-a", true)), "media", "qb")
	if !ok {
		t.Fatal("node-a writes no Egress status")
	}
	want := []v1alpha1.EgressPod{{Name: "qb-0", Node: "node-b", Address: "10.244.5.5", Path: v1alpha1.EgressPathLink}}
	if st.Node != "node-a" || !reflect.DeepEqual(st.Pods, want) {
		t.Errorf("status = node %s pods %+v, want node-a and %+v", st.Node, st.Pods, want)
	}
	if c := findCond(st.Conditions, v1alpha1.ConditionProgrammed); c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonAllPodsRouted {
		t.Errorf("Programmed = %s/%s, want True/AllPodsRouted", c.Status, c.Reason)
	}
}

func TestEgressClassRows(t *testing.T) {
	for node, want := range map[string]bool{"node-a": true, "node-b": true, "node-c": false} {
		_, ok := Compute(egressWorld(node, true)).EgressClassStatus["out"]
		if ok != want {
			t.Errorf("%s contributes a row: %t, want %t", node, ok, want)
		}
	}
}

// TestEgressKeepOnShutdown: a pod node keeps its fail-closed pods blocked when
// the agent stops, so a rollout leaves no window where they leave from home.
func TestEgressKeepOnShutdown(t *testing.T) {
	keep := Compute(egressWorld("node-b", true)).Keep
	pod := netip.MustParseAddr("10.244.5.5")
	if want := []datapath.EgressRule{{SrcAddr: pod, Mark: datapath.EgressBlockedMark}}; !reflect.DeepEqual(keep.Egress, want) {
		t.Errorf("keep egress = %+v, want %+v", keep.Egress, want)
	}
	if len(keep.Rules) != 2 || len(keep.Routes) != 1 || len(keep.ClusterCIDRs) == 0 {
		t.Errorf("keep = %+v, want the blocked rules, the blackhole and the cluster set", keep)
	}
	if k := Compute(egressWorld("node-b", false)).Keep; !reflect.DeepEqual(k, datapath.State{}) {
		t.Errorf("keep = %+v, want nothing without failClosed", k)
	}
}
