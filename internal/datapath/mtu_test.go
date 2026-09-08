package datapath

import (
	"context"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestLinkMTUBetween covers the sizing rule: the smaller of the two underlays
// less the encapsulation, with a peer that has not reported yet contributing
// nothing rather than dragging the link to zero.
func TestLinkMTUBetween(t *testing.T) {
	cases := []struct {
		name       string
		this, peer int
		v4         bool
		want       int
	}{
		{"equal underlays", 1350, 1350, true, 1300},
		{"peer is smaller", 1350, 1280, true, 1230},
		{"this node is smaller", 1280, 1350, true, 1230},
		{"peer has not reported", 1350, 0, true, 1300},
		{"neither has reported", 0, 0, true, 0},
		{"ipv6 outer costs 20 more", 1350, 1350, false, 1280},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LinkMTUBetween(c.this, c.peer, c.v4); got != c.want {
				t.Errorf("LinkMTUBetween(%d, %d, %v) = %d, want %d",
					c.this, c.peer, c.v4, got, c.want)
			}
		})
	}
}

// TestApplyLinksSetsMTU asserts a link is created at the MTU the plan names,
// and that a device already present at the wrong MTU is corrected in place
// rather than rebuilt: the index has to survive, because a rebuild drops the
// /31 and every route pointing at it.
func TestApplyLinksSetsMTU(t *testing.T) {
	state := targetRemoteState(t)
	if len(state.Links) == 0 {
		t.Fatal("target-remote case carries no link")
	}
	for i := range state.Links {
		state.Links[i].MTU = 1300
	}
	plan := Render(state)
	name := state.Links[0].Name

	nl := newFakeNL()
	h := Handles{NFT: &fakeNFT{}, NL: nl}
	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("apply: %v", err)
	}
	created := nl.links[name]
	if created == nil {
		t.Fatalf("link %s was not created", name)
	}
	if got := created.Attrs().MTU; got != 1300 {
		t.Fatalf("created link MTU = %d, want 1300", got)
	}
	idx := created.Attrs().Index

	// The kernel default is what a link created before this change carries.
	created.Attrs().MTU = 1500
	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	corrected := nl.links[name]
	if got := corrected.Attrs().MTU; got != 1300 {
		t.Errorf("MTU after correcting pass = %d, want 1300", got)
	}
	if got := corrected.Attrs().Index; got != idx {
		t.Errorf("device index moved from %d to %d; the link was rebuilt", idx, got)
	}
}

// TestApplyLinksLeavesMTUUnsetWhenZero asserts a plan carrying no MTU makes no
// MTU call, so a link keeps whatever the kernel gave it while the peer's
// status row is still missing.
func TestApplyLinksLeavesMTUUnsetWhenZero(t *testing.T) {
	state := targetRemoteState(t)
	for i := range state.Links {
		state.Links[i].MTU = 0
	}
	plan := Render(state)
	name := state.Links[0].Name

	nl := newFakeNL()
	h := Handles{NFT: &fakeNFT{}, NL: nl}
	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("apply: %v", err)
	}
	nl.links[name].Attrs().MTU = 1500
	before := nl.muts
	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if nl.muts != before {
		t.Errorf("a zero MTU issued %d netlink mutations, want 0", nl.muts-before)
	}
	if got := nl.links[name].Attrs().MTU; got != 1500 {
		t.Errorf("MTU = %d, want the 1500 left in place", got)
	}
}

// TestVxlanForCarriesMTU asserts the create path puts the figure on the device
// rather than relying on a follow-up call, so a link is never briefly live at
// the wrong size.
func TestVxlanForCarriesMTU(t *testing.T) {
	var l netlink.Link = vxlanFor(Link{Name: "kup-test", MTU: 1300})
	if got := l.Attrs().MTU; got != 1300 {
		t.Errorf("vxlanFor MTU = %d, want 1300", got)
	}
}
