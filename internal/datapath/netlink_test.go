package datapath

import (
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
)

// desiredPeerLink is one return link as the reconcile would emit it, with
// LocalAddr varying per call site so the stale-underlay case reads as "the
// node's InternalIP changed".
func desiredPeerLink(local string) Link {
	return Link{
		Name:       "kup-peer0000",
		VNI:        4242,
		Port:       4790,
		LocalAddr:  netip.MustParseAddr(local),
		RemoteAddr: netip.MustParseAddr("10.0.0.9"),
		LinkAddr:   netip.MustParsePrefix("169.254.77.1/31"),
	}
}

// seedStoredLink puts an existing kup-* link into the fake's world, up and
// already carrying the plan's link address, so the only thing left to
// reconcile is the underlay itself.
func seedStoredLink(t *testing.T, f *fakeNL, local, remote net.IP, vni, port int) {
	t.Helper()
	v := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: "kup-peer0000", Index: 7, Flags: net.FlagUp},
		VxlanId:   vni,
		SrcAddr:   local,
		Group:     remote,
		Port:      port,
	}
	f.links[v.Name] = v
	addr, err := netlink.ParseAddr("169.254.77.1/31")
	if err != nil {
		t.Fatalf("parse seed address: %v", err)
	}
	f.addrs[v.Name] = []netlink.Addr{{IPNet: addr.IPNet}}
}

// storedVxlan fetches the link the fake holds back as a VXLAN device.
func storedVxlan(t *testing.T, f *fakeNL) *netlink.Vxlan {
	t.Helper()
	v, ok := f.links["kup-peer0000"].(*netlink.Vxlan)
	if !ok {
		t.Fatalf("stored link is %T, want *netlink.Vxlan", f.links["kup-peer0000"])
	}
	return v
}

// TestApplyLinksUpdatesStaleUnderlay is the F7 contract: a node whose
// InternalIP changed must see its existing kup-* link's underlay move to the
// desired local address on the next apply, in place, without a delete or a
// renumber.
func TestApplyLinksUpdatesStaleUnderlay(t *testing.T) {
	f := newFakeNL()
	seedStoredLink(t, f, net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.9"), 4242, 4790)

	if err := applyLinks(f, []Link{desiredPeerLink("10.0.0.2")}); err != nil {
		t.Fatalf("applyLinks: %v", err)
	}

	stored := storedVxlan(t, f)
	if !stored.SrcAddr.Equal(net.ParseIP("10.0.0.2")) {
		t.Errorf("existing link kept stale underlay: SrcAddr = %v, want 10.0.0.2", stored.SrcAddr)
	}
	if got := stored.Attrs().Index; got != 7 {
		t.Errorf("link index = %d, want 7: the update must be in place, not a recreate", got)
	}
}

// TestApplyLinksUpdatesEachUnderlayParamAlone pins that every param of the
// underlay forces its own reconciliation on its own: an endpoint move
// retargets the device in place (same index), while an identity change —
// VNI or port, which no kernel can change on an existing device — rebuilds
// it (new index, same name), and both end with the desired underlay.
func TestApplyLinksUpdatesEachUnderlayParamAlone(t *testing.T) {
	cases := []struct {
		name              string
		seedLocal, seedRe string
		vni, port         int
		inPlace           bool
	}{
		{"remote differs", "10.0.0.2", "10.0.0.8", 4242, 4790, true},
		{"vni differs", "10.0.0.2", "10.0.0.9", 7, 4790, false},
		{"port differs", "10.0.0.2", "10.0.0.9", 4242, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeNL()
			seedStoredLink(t, f, net.ParseIP(c.seedLocal), net.ParseIP(c.seedRe), c.vni, c.port)

			if err := applyLinks(f, []Link{desiredPeerLink("10.0.0.2")}); err != nil {
				t.Fatalf("applyLinks: %v", err)
			}

			stored := storedVxlan(t, f)
			if !stored.SrcAddr.Equal(net.ParseIP("10.0.0.2")) || !stored.Group.Equal(net.ParseIP("10.0.0.9")) ||
				stored.VxlanId != 4242 || stored.Port != 4790 {
				t.Errorf("underlay not reconciled to desired: local=%v group=%v vni=%d port=%d",
					stored.SrcAddr, stored.Group, stored.VxlanId, stored.Port)
			}
			moved := stored.Attrs().Index != 7
			if c.inPlace && moved {
				t.Errorf("endpoint change recreated the device (index %d): must retarget in place", stored.Attrs().Index)
			}
			if !c.inPlace && !moved {
				t.Error("identity change kept the device index: the kernel cannot modify it in place")
			}
		})
	}
}

// TestApplyLinksLeavesMatchingUnderlayAlone is the other half of F7: the
// comparison must not churn. A link whose underlay already matches is not
// touched at all.
func TestApplyLinksLeavesMatchingUnderlayAlone(t *testing.T) {
	f := newFakeNL()
	seedStoredLink(t, f, net.ParseIP("10.0.0.2"), net.ParseIP("10.0.0.9"), 4242, 4790)

	if err := applyLinks(f, []Link{desiredPeerLink("10.0.0.2")}); err != nil {
		t.Fatalf("applyLinks: %v", err)
	}
	if f.muts != 0 {
		t.Errorf("matching link caused %d mutations, want 0", f.muts)
	}
}

// linkOf builds an existing link with the given underlay for the decision
// table; it is the shape underlayOf reads back from the kernel.
func linkOf(h underlay) netlink.Link {
	return &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: "kup-peer0000"},
		VxlanId:   h.vni,
		SrcAddr:   h.local,
		Group:     h.remote,
		Port:      h.port,
	}
}

// TestLinkActionFor is the F7 decision table run directly against the pure
// comparison: identical params are a no-op; each endpoint differing alone
// retargets; each identity param differing alone — or an underlay that
// cannot be read — rebuilds.
func TestLinkActionFor(t *testing.T) {
	want := desiredPeerLink("10.0.0.2")
	match := underlay{
		local:  net.ParseIP("10.0.0.2"),
		remote: net.ParseIP("10.0.0.9"),
		vni:    4242,
		port:   4790,
	}
	cases := []struct {
		name string
		link netlink.Link
		want linkAction
	}{
		{"identical", linkOf(match), linkKeep},
		// The kernel dumps v4 addrs in the 16-byte v4-mapped shape; that is
		// the same address, not a change.
		{"local in v4-mapped form", linkOf(underlay{local: net.IPv4(10, 0, 0, 2).To16(), remote: match.remote, vni: 4242, port: 4790}), linkKeep},
		{"local differs", linkOf(underlay{local: net.ParseIP("10.0.0.1"), remote: match.remote, vni: 4242, port: 4790}), linkRetarget},
		{"remote differs", linkOf(underlay{local: match.local, remote: net.ParseIP("10.0.0.8"), vni: 4242, port: 4790}), linkRetarget},
		{"local never bound", linkOf(underlay{remote: match.remote, vni: 4242, port: 4790}), linkRebuild},
		{"endpoint family flips", linkOf(underlay{local: net.ParseIP("fd00::5"), remote: match.remote, vni: 4242, port: 4790}), linkRebuild},
		{"vni differs", linkOf(underlay{local: match.local, remote: match.remote, vni: 7, port: 4790}), linkRebuild},
		{"port differs", linkOf(underlay{local: match.local, remote: match.remote, vni: 4242, port: 1}), linkRebuild},
		{"not a vxlan", &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "kup-strange"}}, linkRebuild},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := linkActionFor(c.link, want); got != c.want {
				t.Errorf("linkActionFor(%s) = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

// TestUnderlayOf covers the extraction: a VXLAN link reads back its four
// underlay params from the fields the library fills on a kernel dump, and a
// device that is not a VXLAN reports false — which linkActionFor turns into
// a rebuild, the only way to own a kup-* name that holds the wrong kind of
// device.
func TestUnderlayOf(t *testing.T) {
	v := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: "kup-peer0000"},
		VxlanId:   4242,
		SrcAddr:   net.ParseIP("10.0.0.2"),
		Group:     net.ParseIP("10.0.0.9"),
		Port:      4790,
	}
	have, ok := underlayOf(v)
	if !ok {
		t.Fatal("underlayOf(Vxlan) = false, want true")
	}
	if !have.local.Equal(v.SrcAddr) || !have.remote.Equal(v.Group) || have.vni != 4242 || have.port != 4790 {
		t.Errorf("underlayOf = %+v, want {10.0.0.2 10.0.0.9 4242 4790}", have)
	}

	if _, ok := underlayOf(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "kup-strange"}}); ok {
		t.Error("underlayOf(non-Vxlan) = true, want false")
	}
}

// TestApplyLinksNoSetterStillConverges pins the seam promise: a connection
// that does not provide the in-place retarget capability must still end up
// at the desired underlay — via the rebuild path — so widening the optional
// interface never stranded an existing consumer's fake.
func TestApplyLinksNoSetterStillConverges(t *testing.T) {
	f := newFakeNL()
	seedStoredLink(t, f, net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.9"), 4242, 4790)

	// Embedding the interface promotes exactly its method set: noSetter
	// therefore cannot reach underlaySetter, whatever the concrete fake holds.
	type noSetter struct{ NetlinkConn }

	if err := applyLinks(noSetter{f}, []Link{desiredPeerLink("10.0.0.2")}); err != nil {
		t.Fatalf("applyLinks: %v", err)
	}
	stored := storedVxlan(t, f)
	if !stored.SrcAddr.Equal(net.ParseIP("10.0.0.2")) {
		t.Errorf("underlay did not converge: local = %v, want 10.0.0.2", stored.SrcAddr)
	}
	if stored.Attrs().Index == 7 {
		t.Error("index held at the seeded 7 without the setter — rebuild expected instead")
	}
}

// TestLinkActionForNormalisesDesiredForms pins that a desired endpoint in the
// v4-mapped shape matches the kernel's 4-byte dump: an unnormalised compare
// would otherwise re-fire an update on every apply forever.
func TestLinkActionForNormalisesDesiredForms(t *testing.T) {
	want := desiredPeerLink("10.0.0.2")
	mapped, ok := netip.AddrFromSlice(net.ParseIP("10.0.0.2").To16())
	if !ok {
		t.Fatal("cannot build v4-mapped local")
	}
	want.LocalAddr = mapped
	mapped, ok = netip.AddrFromSlice(net.ParseIP("10.0.0.9").To16())
	if !ok {
		t.Fatal("cannot build v4-mapped remote")
	}
	want.RemoteAddr = mapped

	existing := linkOf(underlay{
		local:  net.ParseIP("10.0.0.2"),
		remote: net.ParseIP("10.0.0.9"),
		vni:    4242,
		port:   4790,
	})
	if got := linkActionFor(existing, want); got != linkKeep {
		t.Errorf("v4-mapped desired against a 4-byte dump re-reads as %d, want keep", got)
	}
}
