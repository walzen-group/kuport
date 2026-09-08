package reconcile

import (
	"net/netip"
	"testing"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// TestBuildLinkParamsMTU covers where the device's size comes from: both ends'
// reported underlays, read out of the class status, with the smaller winning.
// A link built before either node has written a row carries no MTU at all, so
// the device keeps the kernel default until a later pass corrects it.
func TestBuildLinkParamsMTU(t *testing.T) {
	const a, b = "node-a", "node-b"
	subnet := netip.MustParsePrefix("169.254.77.0/24")
	idx := &index{nodeAddr: map[string]string{a: "10.0.0.1", b: "10.0.0.2"}}

	row := func(name string, underlay int32) v1alpha1.NodeStatus {
		return v1alpha1.NodeStatus{Name: name, Ready: true, UnderlayMTU: underlay}
	}

	cases := []struct {
		name string
		rows []v1alpha1.NodeStatus
		want uint32
	}{
		{"both nodes at 1350", []v1alpha1.NodeStatus{row(a, 1350), row(b, 1350)}, 1300},
		{"peer is smaller", []v1alpha1.NodeStatus{row(a, 1350), row(b, 1280)}, 1230},
		{"this node is smaller", []v1alpha1.NodeStatus{row(a, 1280), row(b, 1350)}, 1230},
		{"peer has written no row", []v1alpha1.NodeStatus{row(a, 1350)}, 1300},
		{"no rows at all", nil, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cls := class("c", []string{a, b}, withNodeRows(c.rows...))
			lp, ok := buildLinkParams(idx, cls, subnet, a, b, 0)
			if !ok {
				t.Fatal("buildLinkParams returned false")
			}
			if lp.mtu != c.want {
				t.Errorf("mtu = %d, want %d", lp.mtu, c.want)
			}
		})
	}
}

// TestBuildLinkParamsMTUAgreesOnBothEnds asserts the two agents compute the
// same figure for one link. They read the same two status rows, so a
// disagreement would mean one end sending frames the other end's path cannot
// carry.
func TestBuildLinkParamsMTUAgreesOnBothEnds(t *testing.T) {
	const a, b = "node-a", "node-b"
	subnet := netip.MustParsePrefix("169.254.77.0/24")
	idx := &index{nodeAddr: map[string]string{a: "10.0.0.1", b: "10.0.0.2"}}
	cls := class("c", []string{a, b}, withNodeRows(
		v1alpha1.NodeStatus{Name: a, Ready: true, UnderlayMTU: 1350},
		v1alpha1.NodeStatus{Name: b, Ready: true, UnderlayMTU: 1280},
	))

	fromA, ok := buildLinkParams(idx, cls, subnet, a, b, 0)
	if !ok {
		t.Fatal("buildLinkParams from node-a returned false")
	}
	fromB, ok := buildLinkParams(idx, cls, subnet, b, a, 0)
	if !ok {
		t.Fatal("buildLinkParams from node-b returned false")
	}
	if fromA.mtu != fromB.mtu {
		t.Errorf("ends disagree: node-a computed %d, node-b computed %d", fromA.mtu, fromB.mtu)
	}
	if fromA.mtu != 1230 {
		t.Errorf("mtu = %d, want 1230", fromA.mtu)
	}
}
