package reconcile

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// linkWorld is node-a (accepting the public class) with a pod on node-b, run
// from runOn, with the given existing claims and class options.
func linkWorld(runOn string, links []v1alpha1.LinkAllocation, opts ...classOpt) Inputs {
	allOpts := append([]classOpt{withLinks(links...)}, opts...)
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes:    []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"}, allOpts...)},
		PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-b", target: "pod-a"}),
		},
		Now: metav1.NewTime(baseTime),
	}
}

func claimFor(c ClassContribution, key string) (v1alpha1.LinkAllocation, bool) {
	for _, la := range c.ClaimLinks {
		if la.Key == key {
			return la, true
		}
	}
	return v1alpha1.LinkAllocation{}, false
}

func TestLinkAllocation(t *testing.T) {
	t.Run("fresh claim takes lowest free slot", func(t *testing.T) {
		// No claim for node-a/node-b yet; two unrelated claims hold slots 0 and 2.
		// The endpoint holder (node-b) proposes the lowest free slot, 1.
		in := linkWorld("node-b", []v1alpha1.LinkAllocation{
			claim("x", "y", 0),
			claim("p", "q", 2),
		})
		got, ok := claimFor(Compute(in).ClassStatus["public"], "node-a/node-b")
		if !ok {
			t.Fatal("expected a fresh claim for node-a/node-b")
		}
		if got.Slot != 1 {
			t.Errorf("slot = %d, want 1 (lowest free)", got.Slot)
		}
	})

	t.Run("existing claim reused unchanged", func(t *testing.T) {
		// A claim at slot 7 already exists; no new claim is proposed and the link
		// uses slot 7.
		in := linkWorld("node-b", []v1alpha1.LinkAllocation{claim("node-a", "node-b", 7)})
		res := Compute(in)
		if _, ok := claimFor(res.ClassStatus["public"], "node-a/node-b"); ok {
			t.Error("no fresh claim should be proposed for an existing pair")
		}
		// Slot 7 -> the 7th /31 -> addresses .14/.15; node-b takes the second.
		if len(res.State.Links) != 1 || res.State.Links[0].LinkAddr.String() != "169.254.77.15/31" {
			t.Fatalf("link addr = %v, want 169.254.77.15/31 (slot 7, node-b end)", res.State.Links)
		}
	})

	t.Run("subnet exhausted", func(t *testing.T) {
		// A single-slot subnet with two distinct pairs to place.
		in := Inputs{
			NodeName: "node-a",
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
				node("node-b", "10.0.0.2", nil),
				node("node-c", "10.0.0.3", nil),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes:    []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"}, withSubnet("169.254.77.0/31"))},
			PortMaps: []*v1alpha1.PortMap{
				pm("games", "a", 3000, 0, withService("a", "svc")),
				pm("games", "b", 3001, 0, withService("b", "svc")),
			},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.1.1", node: "node-b", target: "pod-a"}),
				slice("games", "b", endpointSpec{addr: "10.244.2.2", node: "node-c", target: "pod-b"}),
			},
			Now: metav1.NewTime(baseTime),
		}
		cond := findCond(Compute(in).ClassStatus["public"].Conditions, v1alpha1.ConditionReady)
		if cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonSubnetExhausted {
			t.Fatalf("Ready = %s/%s, want False/SubnetExhausted", cond.Status, cond.Reason)
		}
	})

	t.Run("/31 address split by node name order", func(t *testing.T) {
		links := []v1alpha1.LinkAllocation{claim("node-a", "node-b", 0)}
		// node-a is the bytewise-lower name and takes the first address.
		a := Compute(linkWorld("node-a", links))
		if got := a.State.Links[0].LinkAddr.String(); got != "169.254.77.0/31" {
			t.Errorf("node-a link addr = %s, want 169.254.77.0/31 (lower name, first address)", got)
		}
		b := Compute(linkWorld("node-b", links))
		if got := b.State.Links[0].LinkAddr.String(); got != "169.254.77.1/31" {
			t.Errorf("node-b link addr = %s, want 169.254.77.1/31 (higher name, second address)", got)
		}
	})
}

func TestLinkGC(t *testing.T) {
	// A world where node-a is selected but nothing needs the pair, so the claim
	// is a GC candidate. No slices, so no mapping needs any link.
	gcWorld := func(la v1alpha1.LinkAllocation) Inputs {
		return Inputs{
			NodeName:   "node-a",
			Nodes:      []*corev1.Node{node("node-a", "10.0.0.1", map[string]string{"edge": "true"})},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes:    []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"}, withLinks(la))},
			Now:        metav1.NewTime(baseTime),
		}
	}

	t.Run("needed pair clears UnusedSince", func(t *testing.T) {
		set := metav1.NewTime(baseTime.Add(-time.Hour))
		in := linkWorld("node-a", []v1alpha1.LinkAllocation{unusedClaim("node-a", "node-b", 0, &set)})
		got, ok := claimFor(Compute(in).ClassStatus["public"], "node-a/node-b")
		if !ok {
			t.Fatal("expected an updated claim clearing UnusedSince")
		}
		if got.UnusedSince != nil {
			t.Errorf("UnusedSince = %v, want nil (pair is needed)", got.UnusedSince)
		}
	})

	t.Run("unneeded pair sets UnusedSince", func(t *testing.T) {
		got, ok := claimFor(Compute(gcWorld(claim("node-a", "node-b", 0))).ClassStatus["public"], "node-a/node-b")
		if !ok || got.UnusedSince == nil {
			t.Fatalf("expected UnusedSince set to now, got %+v ok=%v", got, ok)
		}
		if !got.UnusedSince.Equal(&in0now) {
			t.Errorf("UnusedSince = %v, want now %v", got.UnusedSince, in0now)
		}
	})

	t.Run("stale entry dropped", func(t *testing.T) {
		old := metav1.NewTime(baseTime.Add(-25 * time.Hour))
		res := Compute(gcWorld(unusedClaim("node-a", "node-b", 0, &old)))
		drops := res.ClassStatus["public"].DropLinks
		if len(drops) != 1 || drops[0] != "node-a/node-b" {
			t.Fatalf("DropLinks = %v, want [node-a/node-b]", drops)
		}
	})

	t.Run("23h entry kept", func(t *testing.T) {
		recent := metav1.NewTime(baseTime.Add(-23 * time.Hour))
		res := Compute(gcWorld(unusedClaim("node-a", "node-b", 0, &recent)))
		c := res.ClassStatus["public"]
		if len(c.DropLinks) != 0 {
			t.Errorf("DropLinks = %v, want none (23h < 24h)", c.DropLinks)
		}
		if _, ok := claimFor(c, "node-a/node-b"); ok {
			t.Errorf("no claim update expected: UnusedSince already set and not stale")
		}
	})
}

var in0now = metav1.NewTime(baseTime)
