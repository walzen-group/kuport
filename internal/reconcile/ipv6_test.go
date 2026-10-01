package reconcile

import (
	"math/rand"
	"net/netip"
	"reflect"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// The IPv6 addresses the dual-stack fixtures give the pods the IPv4 fixtures
// already use.
const (
	pod5v6 = "fd00:10:244:5::5"
	pod9v6 = "fd00:10:244:9::9"
)

// dualStack turns a fixture's Service into a PreferDualStack one, adding the
// IPv6 slice it would then have, and turns IPv6 forwarding on for the node the
// pass runs on.
func dualStack(in Inputs, eps ...endpointSpec) Inputs {
	in.Slices = append(in.Slices, slice6("games", "a", eps...))
	in.IPv6Forwarding = true
	return in
}

func addrPtr(s string) *netip.Addr {
	a := netip.MustParseAddr(s)
	return &a
}

var tcp3000 = datapath.PortSel{Proto: "tcp", First: 3000, Last: 3000}

// TestSingleStackStateUnchanged: a Service with only an IPv4 slice yields the
// State it always did, on every node of every fixture, whether or not the node
// forwards IPv6. IPv6 is decided by the Service and by nothing else.
func TestSingleStackStateUnchanged(t *testing.T) {
	worlds := map[string]func(string, ...classOpt) Inputs{"single": remoteWorld, "multi": multiWorld}
	for mode, world := range worlds {
		for _, n := range []string{"node-a", "node-b", "node-c"} {
			before := Compute(world(n)).State
			in := world(n)
			in.IPv6Forwarding = true
			after := Compute(in).State
			if !reflect.DeepEqual(before, after) {
				t.Errorf("%s on %s: state changed with ipv6 forwarding on\nbefore %+v\nafter  %+v", mode, n, before, after)
			}
		}
	}
}

// TestDualStackSameNode: the pod on the accepting node gets a DNAT per
// interface in each family, straight to the pod, and an exemption in each.
func TestDualStackSameNode(t *testing.T) {
	in := dualStack(localWorld("node-a"),
		endpointSpec{addr: "fd00:10:244:5::5", node: "node-a", target: "pod-a"})
	res := Compute(in)

	want := datapath.State{
		DNAT: []datapath.DNATRule{
			{Iface: "eth0", Port: tcp3000, ToAddr: netip.MustParseAddr("10.244.5.5")},
			{Iface: "eth0", Port: tcp3000, ToAddr: netip.MustParseAddr(pod5v6)},
		},
		Exempt: []datapath.ExemptRule{
			{OifName: "cilium_host", DstAddr: addrPtr("10.244.5.5"), Port: tcp3000},
			{OifName: "cilium_host", DstAddr: addrPtr(pod5v6), Port: tcp3000},
		},
	}
	if !reflect.DeepEqual(res.State, want) {
		t.Errorf("state =\n%+v\nwant\n%+v", res.State, want)
	}

	st := res.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]
	if st.Endpoint == nil || st.Endpoint.Address6 != pod5v6 {
		t.Errorf("status endpoint = %+v, want address6 %s", st.Endpoint, pod5v6)
	}
}

// TestDualStackRemoteSingle is the cross-node shape under Single, both ends.
// The accepting node translates each family to the far end of its link in
// that family; the pod's node completes each translation, marks the replies
// of both, and diverts them with one mark into one table that holds a route
// per family. The loop guard stays IPv4 alone.
func TestDualStackRemoteSingle(t *testing.T) {
	world := func(n string) Inputs {
		return dualStack(remoteWorld(n), endpointSpec{addr: pod5v6, node: "node-b", target: "pod-a"})
	}

	toB := linkName("public", "node-b")
	gotA := Compute(world("node-a")).State
	wantA := datapath.State{
		DNAT: []datapath.DNATRule{
			{Iface: "eth0", Port: tcp3000, ToAddr: netip.MustParseAddr("169.254.77.1")},
			{Iface: "eth0", Port: tcp3000, ToAddr: netip.MustParseAddr("fd64:f5ac:e961::1")},
		},
		Exempt: []datapath.ExemptRule{
			{OifName: toB, DstAddr: addrPtr("169.254.77.1"), Port: tcp3000},
			{OifName: toB, DstAddr: addrPtr("fd64:f5ac:e961::1"), Port: tcp3000},
		},
		Links: []datapath.Link{{
			Name: toB, VNI: 4242, Port: 4790,
			LocalAddr:  netip.MustParseAddr("10.0.0.1"),
			RemoteAddr: netip.MustParseAddr("10.0.0.2"),
			LinkAddr:   netip.MustParsePrefix("169.254.77.0/31"),
			LinkAddr6:  netip.MustParsePrefix("fd64:f5ac:e961::/127"),
		}},
	}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Errorf("node-a state =\n%+v\nwant\n%+v", gotA, wantA)
	}

	toA := linkName("public", "node-a")
	guard := netip.MustParsePrefix("10.0.0.1/32")
	gotB := Compute(world("node-b")).State
	wantB := datapath.State{
		DNAT: []datapath.DNATRule{
			{Iface: toA, Port: tcp3000, ToAddr: netip.MustParseAddr("10.244.5.5"), DstAddr: addrPtr("169.254.77.1")},
			{Iface: toA, Port: tcp3000, ToAddr: netip.MustParseAddr(pod5v6), DstAddr: addrPtr("fd64:f5ac:e961::1")},
		},
		Exempt: []datapath.ExemptRule{
			{OifName: "cilium_*", Negate: true, SrcAddr: addrPtr("10.244.5.5"), Port: tcp3000, PortIsSrc: true},
			{OifName: "cilium_*", Negate: true, SrcAddr: addrPtr(pod5v6), Port: tcp3000, PortIsSrc: true},
		},
		Mark: []datapath.MarkRule{
			{SrcAddr: netip.MustParseAddr("10.244.5.5"), Port: tcp3000, Mark: markBase},
			{SrcAddr: netip.MustParseAddr(pod5v6), Port: tcp3000, Mark: markBase},
		},
		Links: []datapath.Link{{
			Name: toA, VNI: 4242, Port: 4790,
			LocalAddr:  netip.MustParseAddr("10.0.0.2"),
			RemoteAddr: netip.MustParseAddr("10.0.0.1"),
			LinkAddr:   netip.MustParsePrefix("169.254.77.1/31"),
			LinkAddr6:  netip.MustParsePrefix("fd64:f5ac:e961::1/127"),
		}},
		Rules: []datapath.IPRule{
			{Pref: 101, Mark: markBase, To: &guard, Table: 254},
			{Pref: 102, Mark: markBase, Table: 200},
			{Pref: 102, Mark: markBase, Table: 200, Family: datapath.FamilyIPv6},
		},
		Routes: []datapath.Route{
			{Table: 200, Via: netip.MustParseAddr("169.254.77.0"), Dev: toA},
			{Table: 200, Via: netip.MustParseAddr("fd64:f5ac:e961::"), Dev: toA},
		},
	}
	if !reflect.DeepEqual(gotB, wantB) {
		t.Errorf("node-b state =\n%+v\nwant\n%+v", gotB, wantB)
	}
}

// TestDualStackMulti: the pod's node translates every link in both families,
// restores the mark for replies of both, and saves it once per link. The save
// rule matches the arriving link alone, which covers both families; a second
// copy would only double the work.
func TestDualStackMulti(t *testing.T) {
	in := dualStack(multiWorld("node-c"), endpointSpec{addr: pod9v6, node: "node-c", target: "pod-a"})
	st := Compute(in).State

	if len(st.CtSave) != 2 {
		t.Errorf("CtSave = %+v, want one per link and not one per family", st.CtSave)
	}
	var load6 bool
	for _, c := range st.CtLoad {
		if c.SrcAddr.String() == pod9v6 {
			load6 = true
		}
	}
	if len(st.CtLoad) != 2 || !load6 {
		t.Errorf("CtLoad = %+v, want a restore for the pod's ipv4 and ipv6 replies", st.CtLoad)
	}
	// Its own interface plus two links, each in two families.
	if len(st.DNAT) != 6 {
		t.Errorf("DNAT = %d rules, want 6", len(st.DNAT))
	}
	for _, l := range st.Links {
		if !l.LinkAddr6.IsValid() {
			t.Errorf("link %s carries no /127", l.Name)
		}
	}
	var divert6 int
	for _, r := range st.Rules {
		if r.Family == datapath.FamilyIPv6 {
			divert6++
			if r.Pref != 102 {
				t.Errorf("ipv6 rule %+v, want only divert rules at pref 102", r)
			}
		}
	}
	if divert6 != 2 {
		t.Errorf("ipv6 divert rules = %d, want one per link", divert6)
	}

	// An accepting node holding no pod forwards each family over its link.
	a := Compute(dualStack(multiWorld("node-a"), endpointSpec{addr: pod9v6, node: "node-c", target: "pod-a"})).State
	if len(a.DNAT) != 2 || a.DNAT[1].ToAddr.String() != "fd64:f5ac:e961::1" {
		t.Errorf("node-a DNAT = %+v, want ipv4 and ipv6 to node-c's ends of slot 0", a.DNAT)
	}
}

// TestIPv6EndpointMustBeTheChosenPod: the IPv4 slices choose the pod as they
// always did, and an IPv6 endpoint counts only when it is that same pod and
// ready. Anything else would send the two families to different pods.
func TestIPv6EndpointMustBeTheChosenPod(t *testing.T) {
	tests := []struct {
		name string
		eps  []endpointSpec
	}{
		{"another pod", []endpointSpec{{addr: "fd00:10:244:6::6", node: "node-a", target: "pod-b"}}},
		{"the pod, not ready", []endpointSpec{{addr: pod5v6, node: "node-a", target: "pod-a", notReady: true}}},
		{"no targetRef", []endpointSpec{{addr: pod5v6, node: "node-a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compute(dualStack(localWorld("node-a"), tt.eps...))
			for _, d := range res.State.DNAT {
				if d.ToAddr.Is6() {
					t.Errorf("DNAT to %s, want no ipv6 rule", d.ToAddr)
				}
			}
			st := res.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]
			if st.Endpoint == nil || st.Endpoint.Address6 != "" {
				t.Errorf("status endpoint = %+v, want no address6", st.Endpoint)
			}
		})
	}

	// The same pod beside another in the IPv6 slice: the pod's own address is
	// the one used.
	res := Compute(dualStack(localWorld("node-a"),
		endpointSpec{addr: "fd00:10:244:6::6", node: "node-a", target: "pod-b"},
		endpointSpec{addr: pod5v6, node: "node-a", target: "pod-a"}))
	if len(res.State.DNAT) != 2 || res.State.DNAT[1].ToAddr.String() != pod5v6 {
		t.Errorf("DNAT = %+v, want the second rule to pod-a's %s", res.State.DNAT, pod5v6)
	}
}

// TestIPv6OnlyServiceIsNotProgrammed keeps today's behaviour for a Service
// with an IPv6 slice and no IPv4 one: the endpoint is chosen from IPv4 slices,
// so there is none, and the mapping says so.
func TestIPv6OnlyServiceIsNotProgrammed(t *testing.T) {
	in := localWorld("node-a")
	in.Slices = []*discoveryv1.EndpointSlice{slice6("games", "a", endpointSpec{addr: pod5v6, node: "node-a", target: "pod-a"})}
	in.IPv6Forwarding = true
	res := Compute(in)
	if len(res.State.DNAT) != 0 {
		t.Errorf("DNAT = %+v, want none", res.State.DNAT)
	}
	if c := programmedOf(res, "games", "a"); c.Reason != v1alpha1.ReasonNoReadyEndpoint {
		t.Errorf("Programmed reason = %s, want NoReadyEndpoint", c.Reason)
	}
}

// TestIPv6ForwardingOff: a node whose host does not forward IPv6 programs the
// IPv4 half alone and says why in its class row. Its IPv4 state is exactly the
// single-stack one.
func TestIPv6ForwardingOff(t *testing.T) {
	for _, n := range []string{"node-a", "node-b"} {
		in := dualStack(remoteWorld(n), endpointSpec{addr: pod5v6, node: "node-b", target: "pod-a"})
		in.IPv6Forwarding = false
		if got, want := Compute(in).State, Compute(remoteWorld(n)).State; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: state with ipv6 forwarding off =\n%+v\nwant the ipv4-only state\n%+v", n, got, want)
		}
	}

	in := dualStack(remoteWorld("node-a"), endpointSpec{addr: pod5v6, node: "node-b", target: "pod-a"})
	in.IPv6Forwarding = false
	in.InterfaceAddrs = map[string]string{"eth0": "203.0.113.9"}
	in.InterfaceAddrs6 = map[string]string{"eth0": "2001:db8::9"}
	res := Compute(in)
	row := res.ClassStatus["public"].Node
	if row.IPv6Unavailable == "" {
		t.Errorf("node row = %+v, want it to say why ipv6 is unavailable", row)
	}
	if !row.Ready {
		t.Errorf("node row = %+v, want it ready: ipv4 is unaffected", row)
	}
	if len(row.Addresses6) != 0 {
		t.Errorf("node row addresses6 = %v, want none while ipv6 is unavailable", row.Addresses6)
	}
	st := res.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]
	if st.Endpoint == nil || st.Endpoint.Address6 != "" {
		t.Errorf("status endpoint = %+v, want no address6 from a node that does not deliver it", st.Endpoint)
	}
}

// TestNodeRowCarriesIPv6Addresses: with forwarding on, the row publishes each
// interface's IPv6 address for the status writer on another node to read.
func TestNodeRowCarriesIPv6Addresses(t *testing.T) {
	in := dualStack(remoteWorld("node-a"), endpointSpec{addr: pod5v6, node: "node-b", target: "pod-a"})
	in.InterfaceAddrs = map[string]string{"eth0": "203.0.113.9"}
	in.InterfaceAddrs6 = map[string]string{"eth0": "2001:db8::9"}
	row := Compute(in).ClassStatus["public"].Node
	if row.Addresses6["eth0"] != "2001:db8::9" || row.IPv6Unavailable != "" {
		t.Errorf("node row = %+v, want addresses6 eth0 2001:db8::9 and no ipv6Unavailable", row)
	}
}

// TestLinkSlash127FollowsTheSlot: slot n takes the nth /127 of subnet6, and the
// node whose name sorts first takes the low address, the way the /31 splits.
func TestLinkSlash127FollowsTheSlot(t *testing.T) {
	links := []v1alpha1.LinkAllocation{claim("node-a", "node-b", 7)}
	world := func(n string, opts ...classOpt) Inputs {
		in := linkWorld(n, links, opts...)
		return dualStack(in, endpointSpec{addr: pod5v6, node: "node-b", target: "pod-a"})
	}

	a := Compute(world("node-a")).State.Links
	b := Compute(world("node-b")).State.Links
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("links = %+v / %+v, want one on each end", a, b)
	}
	if got := a[0].LinkAddr6.String(); got != "fd64:f5ac:e961::e/127" {
		t.Errorf("node-a /127 = %s, want fd64:f5ac:e961::e/127 (slot 7, lower name)", got)
	}
	if got := b[0].LinkAddr6.String(); got != "fd64:f5ac:e961::f/127" {
		t.Errorf("node-b /127 = %s, want fd64:f5ac:e961::f/127 (slot 7, higher name)", got)
	}

	// A class's own subnet6 is used, masked to its prefix the way subnet is.
	c := Compute(world("node-b", withSubnet6("fd12:3456:789a:1::ff05/120"))).State.Links
	if len(c) != 1 || c[0].LinkAddr6.String() != "fd12:3456:789a:1::ff0f/127" {
		t.Errorf("links = %+v, want node-b at fd12:3456:789a:1::ff0f/127", c)
	}
}

// TestLinkBelowIPv6MinimumCarriesNoIPv6: the kernel refuses an IPv6 address on
// a device under 1280 bytes, and the refusal would fail the whole apply,
// IPv4 included. A link sized below that stays IPv4, at both ends.
func TestLinkBelowIPv6MinimumCarriesNoIPv6(t *testing.T) {
	rows := withNodeRows(
		v1alpha1.NodeStatus{Name: "node-a", Ready: true, UnderlayMTU: 1320},
		v1alpha1.NodeStatus{Name: "node-b", Ready: true, UnderlayMTU: 1320},
	)
	for _, n := range []string{"node-a", "node-b"} {
		st := Compute(dualStack(remoteWorld(n, rows), endpointSpec{addr: pod5v6, node: "node-b", target: "pod-a"})).State
		if len(st.Links) != 1 || st.Links[0].MTU != 1270 {
			t.Fatalf("%s: links = %+v, want one at mtu 1270", n, st.Links)
		}
		if st.Links[0].LinkAddr6.IsValid() {
			t.Errorf("%s: link carries %s at mtu 1270", n, st.Links[0].LinkAddr6)
		}
		for _, r := range st.Rules {
			if r.Family == datapath.FamilyIPv6 {
				t.Errorf("%s: ipv6 rule %+v over a link that cannot carry ipv6", n, r)
			}
		}
		for _, d := range st.DNAT {
			if d.ToAddr.Is6() && d.DstAddr != nil {
				t.Errorf("%s: ipv6 link DNAT %+v over a link that cannot carry ipv6", n, d)
			}
			if n == "node-a" && d.ToAddr.Is6() {
				t.Errorf("node-a: ipv6 DNAT %+v toward a link that cannot carry ipv6", d)
			}
		}
	}
}

// TestInvalidSubnet6: a subnet6 the API should have refused leaves IPv4 as it
// is and the class Ready=False naming the field.
func TestInvalidSubnet6(t *testing.T) {
	in := dualStack(remoteWorld("node-a", withSubnet6("169.254.0.0/16")),
		endpointSpec{addr: pod5v6, node: "node-b", target: "pod-a"})
	res := Compute(in)
	if len(res.State.Links) != 1 || res.State.Links[0].LinkAddr6.IsValid() {
		t.Errorf("links = %+v, want the ipv4 link alone", res.State.Links)
	}
	cond := findCond(res.ClassStatus["public"].Conditions, v1alpha1.ConditionReady)
	if cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonInvalidSubnet {
		t.Errorf("Ready = %s/%s, want False/InvalidSubnet", cond.Status, cond.Reason)
	}
}

// TestDualStackDeterminismUnderShuffle: two agents must emit the same State
// for the same inputs, and the IPv4 and IPv6 objects of one mapping tie on
// every key but the family.
func TestDualStackDeterminismUnderShuffle(t *testing.T) {
	base := dualStack(multiWorld("node-c"), endpointSpec{addr: pod9v6, node: "node-c", target: "pod-a"})
	want := Compute(cloneInputs(base))
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 20; i++ {
		if got := Compute(shuffleInputs(cloneInputs(base), rng)); !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle iteration %d produced a different Result", i)
		}
	}
}
