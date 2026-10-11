package reconcile

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// linkWorld is node-a (accepting the public class) with a pod on node-b, run
// from runOn, with the given existing Link objects and class options.
func linkWorld(runOn string, links []*v1alpha1.Link, opts ...classOpt) Inputs {
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", nil),
			node("node-c", "10.0.0.3", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a"}, opts...)},
		PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-b", target: "pod-a"}),
		},
		Links: links,
		Now:   metav1.NewTime(baseTime),
	}
}

func createFor(res Result, kind, className, a, b string) (v1alpha1.Link, bool) {
	p := sortedPair(a, b)
	for _, l := range res.Links.Create {
		if l.Spec.ClassKind == kind && l.Spec.ClassName == className &&
			l.Spec.Peers[0] == p[0] && l.Spec.Peers[1] == p[1] {
			return l, true
		}
	}
	return v1alpha1.Link{}, false
}

func statusFor(res Result, name string) (LinkStatusUpdate, bool) {
	for _, u := range res.Links.Status {
		if u.Name == name {
			return u, true
		}
	}
	return LinkStatusUpdate{}, false
}

func deleteNames(res Result) []string {
	var out []string
	for _, d := range res.Links.Delete {
		out = append(out, d.Name)
	}
	return out
}

func TestLinkAllocation(t *testing.T) {
	t.Run("fresh claim takes the lowest slot free in any class", func(t *testing.T) {
		// Slot 0 belongs to another PortMapClass and slot 2 to an EgressClass.
		// The endpoint holder, node-b, proposes slot 1.
		in := linkWorld("node-b", []*v1alpha1.Link{
			linkOf(v1alpha1.ClassKindPortMapClass, "other", "x", "y", 0),
			linkOf(v1alpha1.ClassKindEgressClass, "egress", "p", "q", 2),
		})
		got, ok := createFor(Compute(in), v1alpha1.ClassKindPortMapClass, "public", "node-a", "node-b")
		if !ok {
			t.Fatal("expected node-b to create a Link for public node-a/node-b")
		}
		if got.Name != "slot-1" {
			t.Errorf("name = %s, want slot-1 (lowest free across classes)", got.Name)
		}
	})

	t.Run("only the endpoint holder creates", func(t *testing.T) {
		in := linkWorld("node-a", nil)
		if res := Compute(in); len(res.Links.Create) != 0 {
			t.Errorf("node-a created %v; only node-b holds the endpoint", res.Links.Create)
		}
	})

	t.Run("existing Link sets every number from its slot", func(t *testing.T) {
		in := linkWorld("node-b", []*v1alpha1.Link{claim("node-a", "node-b", 7)})
		res := Compute(in)
		if len(res.Links.Create) != 0 {
			t.Errorf("created %v for a pair that already has a Link", res.Links.Create)
		}
		if len(res.State.Links) != 1 {
			t.Fatalf("links = %+v, want one", res.State.Links)
		}
		l := res.State.Links[0]
		// Slot 7 is the 7th /31 of 169.254.64.0/21: .14 and .15; node-b, the
		// higher name, gets .15.
		if l.Name != "kup-7" || l.VNI != 7007 || l.Port != 4790 || l.LinkAddr.String() != "169.254.64.15/31" {
			t.Errorf("link = %s vni %d port %d addr %s, want kup-7 vni 7007 port 4790 addr 169.254.64.15/31",
				l.Name, l.VNI, l.Port, l.LinkAddr)
		}
		wantRules := []datapath.IPRule{
			{Pref: 101, Mark: 0x6b700007, To: prefixPtr("10.0.0.1/32"), Table: 254},
			{Pref: 102, Mark: 0x6b700007, Table: 1007},
		}
		if fmt.Sprint(res.State.Rules) != fmt.Sprint(wantRules) {
			t.Errorf("rules = %v, want %v", res.State.Rules, wantRules)
		}
	})

	t.Run("two classes on one node get distinct numbers", func(t *testing.T) {
		// Both classes accept on node-a and both pods run on node-b, the shape
		// where per-class slots gave both links mark 0x6b700000 and table 200.
		in := linkWorld("node-b", []*v1alpha1.Link{
			claim("node-a", "node-b", 0),
			linkOf(v1alpha1.ClassKindPortMapClass, "private", "node-a", "node-b", 1),
		})
		in.Classes = append(in.Classes, class("private", []string{"node-a"}))
		in.PortMaps = append(in.PortMaps, pm("games", "b", 3001, 1, withClass("private")))
		in.Slices = append(in.Slices, slice("games", "b", endpointSpec{addr: "10.244.5.6", node: "node-b", target: "pod-b"}))

		res := Compute(in)
		seen := map[string]string{}
		for _, l := range res.State.Links {
			for _, k := range []string{l.Name, fmt.Sprint(l.VNI), l.LinkAddr.String()} {
				if prev, dup := seen[k]; dup && prev != l.Name {
					t.Errorf("%s and %s share %s", prev, l.Name, k)
				}
				seen[k] = l.Name
			}
		}
		if len(res.State.Links) != 2 {
			t.Fatalf("links = %+v, want two", res.State.Links)
		}
		tables := map[uint32]bool{}
		for _, r := range res.State.Routes {
			tables[r.Table] = true
		}
		if !tables[1000] || !tables[1001] || len(tables) != 2 {
			t.Errorf("route tables = %v, want 1000 and 1001", tables)
		}
	})

	t.Run("no slot lands on the kernel's tables", func(t *testing.T) {
		res := Compute(linkWorld("node-b", []*v1alpha1.Link{claim("node-a", "node-b", 53)}))
		for _, r := range res.State.Routes {
			if r.Table >= 253 && r.Table <= 255 {
				t.Errorf("slot 53 routes in table %d, one of the kernel's", r.Table)
			}
			if r.Table != 1053 {
				t.Errorf("table = %d, want 1053", r.Table)
			}
		}
	})

	t.Run("pool exhausted", func(t *testing.T) {
		var links []*v1alpha1.Link
		for s := int32(0); s < 1024; s++ {
			links = append(links, linkOf(v1alpha1.ClassKindPortMapClass, "other", "x", fmt.Sprintf("y%d", s), s))
		}
		res := Compute(linkWorld("node-a", links))
		cond := findCond(res.ClassStatus["public"].Conditions, v1alpha1.ConditionReady)
		if cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonSubnetExhausted {
			t.Fatalf("Ready = %s/%s, want False/SubnetExhausted", cond.Status, cond.Reason)
		}
		if b := Compute(linkWorld("node-b", links)); len(b.Links.Create) != 0 {
			t.Errorf("node-b created %v with the pool full", b.Links.Create)
		}
	})

	t.Run("/31 address split by node name order", func(t *testing.T) {
		links := []*v1alpha1.Link{claim("node-a", "node-b", 0)}
		a := Compute(linkWorld("node-a", links))
		if got := a.State.Links[0].LinkAddr.String(); got != "169.254.64.0/31" {
			t.Errorf("node-a link addr = %s, want 169.254.64.0/31 (lower name, first address)", got)
		}
		b := Compute(linkWorld("node-b", links))
		if got := b.State.Links[0].LinkAddr.String(); got != "169.254.64.1/31" {
			t.Errorf("node-b link addr = %s, want 169.254.64.1/31 (higher name, second address)", got)
		}
	})

	t.Run("duplicate Links resolve to the lower slot", func(t *testing.T) {
		links := []*v1alpha1.Link{claim("node-a", "node-b", 0), claim("node-a", "node-b", 3)}
		b := Compute(linkWorld("node-b", links))
		if len(b.State.Links) != 1 || b.State.Links[0].Name != "kup-0" {
			t.Fatalf("links = %+v, want kup-0 alone", b.State.Links)
		}
		if got := deleteNames(b); len(got) != 1 || got[0] != "slot-3" {
			t.Errorf("node-b deletes %v, want [slot-3]: it proposed the pair", got)
		}
		if b.Links.Delete[0].ResourceVersion != "rv-slot-3" {
			t.Errorf("delete precondition = %q, want the cached rv-slot-3", b.Links.Delete[0].ResourceVersion)
		}
		if a := Compute(linkWorld("node-a", links)); len(a.Links.Delete) != 0 {
			t.Errorf("node-a deletes %v; only the proposer removes a duplicate", deleteNames(a))
		}
	})

	t.Run("link port comes from the agent", func(t *testing.T) {
		in := linkWorld("node-b", []*v1alpha1.Link{claim("node-a", "node-b", 0)})
		in.LinkPort = 4800
		if got := Compute(in).State.Links[0].Port; got != 4800 {
			t.Errorf("port = %d, want 4800 from Inputs.LinkPort", got)
		}
	})

	t.Run("deprecated vxlan fields are ignored", func(t *testing.T) {
		in := linkWorld("node-b", []*v1alpha1.Link{claim("node-a", "node-b", 0)},
			withSubnet("169.254.78.0/24"), withSubnet6("fd00::/112"))
		l := Compute(in).State.Links[0]
		if l.LinkAddr.String() != "169.254.64.1/31" || l.VNI != 7000 {
			t.Errorf("link = %s vni %d, want 169.254.64.1/31 vni 7000 from the pool", l.LinkAddr, l.VNI)
		}
	})
}

func TestLinkRelease(t *testing.T) {
	// No slices, so no mapping needs any link. node-c is a peer of nothing.
	gcWorld := func(runOn string, l *v1alpha1.Link) Inputs {
		in := linkWorld(runOn, []*v1alpha1.Link{l})
		in.Slices = nil
		return in
	}

	t.Run("needed Link clears UnusedSince", func(t *testing.T) {
		set := metav1.NewTime(baseTime.Add(-time.Hour))
		in := linkWorld("node-a", []*v1alpha1.Link{unusedClaim("node-a", "node-b", 0, &set)})
		got, ok := statusFor(Compute(in), "slot-0")
		if !ok {
			t.Fatal("expected a status update clearing UnusedSince")
		}
		if got.UnusedSince != nil {
			t.Errorf("UnusedSince = %v, want nil (the pair is needed)", got.UnusedSince)
		}
	})

	t.Run("unneeded Link gets UnusedSince from any agent", func(t *testing.T) {
		for _, runOn := range []string{"node-a", "node-b", "node-c"} {
			got, ok := statusFor(Compute(gcWorld(runOn, claim("node-a", "node-b", 0))), "slot-0")
			if !ok || got.UnusedSince == nil || !got.UnusedSince.Equal(&in0now) {
				t.Errorf("%s: update = %+v ok=%v, want UnusedSince = now", runOn, got, ok)
			}
			if got.ResourceVersion != "rv-slot-0" {
				t.Errorf("%s: precondition = %q, want rv-slot-0", runOn, got.ResourceVersion)
			}
		}
	})

	t.Run("Link unused for more than 24h is deleted", func(t *testing.T) {
		old := metav1.NewTime(baseTime.Add(-25 * time.Hour))
		res := Compute(gcWorld("node-c", unusedClaim("node-a", "node-b", 0, &old)))
		if got := deleteNames(res); len(got) != 1 || got[0] != "slot-0" {
			t.Fatalf("deletes = %v, want [slot-0]", got)
		}
	})

	t.Run("Link unused for 23h is kept untouched", func(t *testing.T) {
		recent := metav1.NewTime(baseTime.Add(-23 * time.Hour))
		res := Compute(gcWorld("node-a", unusedClaim("node-a", "node-b", 0, &recent)))
		if len(res.Links.Delete) != 0 || len(res.Links.Status) != 0 {
			t.Errorf("deletes %v, status %v; want neither at 23h", deleteNames(res), res.Links.Status)
		}
	})

	t.Run("a Link with a name outside the pool is left alone", func(t *testing.T) {
		l := claim("node-a", "node-b", 0)
		l.Name = "not-a-slot"
		res := Compute(gcWorld("node-a", l))
		if len(res.Links.Status) != 0 || len(res.Links.Delete) != 0 {
			t.Errorf("touched %q: status %v, deletes %v", l.Name, res.Links.Status, deleteNames(res))
		}
	})
}

func TestLegacyClassLinksCleared(t *testing.T) {
	in := linkWorld("node-a", nil, withLegacyLinks(v1alpha1.LinkAllocation{Key: "node-a/node-b", Slot: 0}))
	if !Compute(in).ClassStatus["public"].ClearLegacyLinks {
		t.Error("the accepting node should clear the class's status.links")
	}
	if Compute(linkWorld("node-a", nil)).ClassStatus["public"].ClearLegacyLinks {
		t.Error("a class without status.links needs no clearing")
	}
}

func TestDeprecatedVxlanCondition(t *testing.T) {
	in := linkWorld("node-a", nil, withSubnet("169.254.78.0/24"), withSubnet6("fd00::/112"))
	cond := findCond(Compute(in).ClassStatus["public"].Conditions, v1alpha1.ConditionDeprecated)
	if cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonDeprecatedField {
		t.Fatalf("Deprecated = %s/%s, want True/DeprecatedField", cond.Status, cond.Reason)
	}
	for _, f := range []string{"subnet", "subnet6"} {
		if !strings.Contains(cond.Message, "returnPath.vxlan."+f) {
			t.Errorf("message %q does not name returnPath.vxlan.%s", cond.Message, f)
		}
	}
	clean := findCond(Compute(linkWorld("node-a", nil)).ClassStatus["public"].Conditions, v1alpha1.ConditionDeprecated)
	if clean.Status != metav1.ConditionFalse {
		t.Errorf("Deprecated = %s for a class setting nothing, want False", clean.Status)
	}
}

// TestLinkName: the device name follows the slot, which is unique cluster-wide,
// so two classes on one node can never share a device.
func TestLinkName(t *testing.T) {
	if got := linkName(0); got != "kup-0" {
		t.Errorf("linkName(0) = %q, want kup-0", got)
	}
	// The kernel caps interface names at 15 characters.
	if got := linkName(1023); got != "kup-1023" || len(got) > 15 {
		t.Errorf("linkName(1023) = %q, want kup-1023", got)
	}
}

func TestLinkIPv6Ends(t *testing.T) {
	lo, hi := nthSlash127(linkSubnet6, 0)
	if lo.String() != "fd64:f5ac:e961::1:0" || hi.String() != "fd64:f5ac:e961::1:1" {
		t.Errorf("slot 0 /127 = %s, %s; want fd64:f5ac:e961::1:0 and ::1:1", lo, hi)
	}
}

var in0now = metav1.NewTime(baseTime)
