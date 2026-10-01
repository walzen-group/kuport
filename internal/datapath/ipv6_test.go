package datapath

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func dualStackTargetState(t *testing.T) State {
	t.Helper()
	for _, c := range cases() {
		if c.name == "target-remote-dual-stack" {
			return c.state
		}
	}
	t.Fatal("target-remote-dual-stack case missing")
	return State{}
}

// linkAddrs lists a link's addresses in the fake's world as prefix strings.
func linkAddrs(f *fakeNL, name string) map[string]bool {
	out := map[string]bool{}
	for _, a := range f.addrs[name] {
		out[a.IPNet.String()] = true
	}
	return out
}

func hasRule(f *fakeNL, family, pref int, mark uint32, table int) bool {
	for _, r := range f.rules {
		if r.Family == family && r.Priority == pref && r.Mark == mark && r.Table == table {
			return true
		}
	}
	return false
}

func hasRoute(f *fakeNL, family, table int, via string) bool {
	for _, r := range f.routes {
		if r.Family == family && r.Table == table && r.Gw.Equal(net.ParseIP(via)) {
			return true
		}
	}
	return false
}

// TestApplyCarriesIPv6 applies the dual-stack target state: the link holds its
// /127 beside its /31, the divert rule exists in both families with the same
// mark and table, the return table has a default route in each family via the
// peer's end, and the loop guard stays IPv4 alone, since the outer packet that
// could loop is always IPv4. A second apply changes nothing.
func TestApplyCarriesIPv6(t *testing.T) {
	nl := newFakeNL()
	h := Handles{NFT: &fakeNFT{}, NL: nl}
	plan := Render(dualStackTargetState(t))

	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("apply: %v", err)
	}

	addrs := linkAddrs(nl, "kup-da0d9a1d")
	for _, want := range []string{"169.254.77.1/31", "fd64:f5ac:e961::1/127"} {
		if !addrs[want] {
			t.Errorf("link addresses = %v, want %s on it", addrs, want)
		}
	}

	if !hasRule(nl, unix.AF_INET, 101, 0x6b700001, 254) || !hasRule(nl, unix.AF_INET, 102, 0x6b700001, 200) {
		t.Errorf("ipv4 rules = %+v, want the loop guard and the divert", nl.rules)
	}
	if !hasRule(nl, unix.AF_INET6, 102, 0x6b700001, 200) {
		t.Errorf("rules = %+v, want an ipv6 divert at pref 102, mark 0x6b700001, table 200", nl.rules)
	}
	if hasRule(nl, unix.AF_INET6, 101, 0x6b700001, 254) {
		t.Errorf("rules = %+v, want no ipv6 loop guard: the underlay is ipv4", nl.rules)
	}

	if !hasRoute(nl, unix.AF_INET, 200, "169.254.77.0") {
		t.Errorf("routes = %+v, want ipv4 default via 169.254.77.0 in table 200", nl.routes)
	}
	if !hasRoute(nl, unix.AF_INET6, 200, "fd64:f5ac:e961::") {
		t.Errorf("routes = %+v, want ipv6 default via fd64:f5ac:e961:: in table 200", nl.routes)
	}

	before := nl.muts
	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if delta := nl.muts - before; delta != 0 {
		t.Errorf("second apply issued %d netlink mutations, want 0", delta)
	}
}

// TestApplyRemovesStaleIPv6 seeds the world with IPv6 objects under kuport's
// names that the plan no longer wants: a /127 from an older subnet6 on its
// link, a divert rule carrying a kuport mark for a slot nothing uses, and a
// route in its return table via an address the link stopped holding. The
// apply removes each of them and leaves alone the link-local address the
// kernel gave the device and a rule whose mark kuport does not own.
func TestApplyRemovesStaleIPv6(t *testing.T) {
	nl := newFakeNL()
	h := Handles{NFT: &fakeNFT{}, NL: nl}
	plan := Render(dualStackTargetState(t))

	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	link, _ := nl.LinkByName("kup-da0d9a1d")
	for _, cidr := range []string{"fd64:aaaa::1/127", "fe80::1c00:ff:fe00:1/64"} {
		a, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatal(err)
		}
		nl.addrs["kup-da0d9a1d"] = append(nl.addrs["kup-da0d9a1d"], *a)
	}
	nl.rules = append(nl.rules,
		netlink.Rule{Family: unix.AF_INET6, Priority: 102, Mark: 0x6b70ffff, Table: 455},
		netlink.Rule{Family: unix.AF_INET6, Priority: 110, Mark: 0x1234, Table: 99},
	)
	nl.routes = append(nl.routes, netlink.Route{
		Family: unix.AF_INET6, Table: 200, LinkIndex: link.Attrs().Index, Gw: net.ParseIP("fd64:aaaa::"),
	})

	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	addrs := linkAddrs(nl, "kup-da0d9a1d")
	if addrs["fd64:aaaa::1/127"] {
		t.Errorf("link addresses = %v, want the stale /127 removed", addrs)
	}
	if !addrs["fe80::1c00:ff:fe00:1/64"] {
		t.Errorf("link addresses = %v, want the kernel's link-local address kept", addrs)
	}
	if !addrs["fd64:f5ac:e961::1/127"] || !addrs["169.254.77.1/31"] {
		t.Errorf("link addresses = %v, want the plan's /31 and /127 kept", addrs)
	}
	if hasRule(nl, unix.AF_INET6, 102, 0x6b70ffff, 455) {
		t.Error("a stale ipv6 rule with a kuport mark was not removed")
	}
	if !hasRule(nl, unix.AF_INET6, 110, 0x1234, 99) {
		t.Error("an ipv6 rule kuport does not own was removed")
	}
	if hasRoute(nl, unix.AF_INET6, 200, "fd64:aaaa::") {
		t.Error("a stale ipv6 route in kuport's return table was not removed")
	}
}

// TestApplyDropsIPv6WhenThePlanHasNone covers a node that stops delivering
// IPv6, because its forwarding was turned off or the Service went back to a
// single stack: the next plan carries IPv4 alone, and every IPv6 object the
// last one wrote goes.
func TestApplyDropsIPv6WhenThePlanHasNone(t *testing.T) {
	nl := newFakeNL()
	h := Handles{NFT: &fakeNFT{}, NL: nl}
	if err := Apply(context.Background(), Render(dualStackTargetState(t)), h); err != nil {
		t.Fatalf("dual-stack apply: %v", err)
	}
	if err := Apply(context.Background(), Render(targetRemoteState(t)), h); err != nil {
		t.Fatalf("ipv4 apply: %v", err)
	}

	for a := range linkAddrs(nl, "kup-da0d9a1d") {
		if p := netip.MustParsePrefix(a); p.Addr().Is6() {
			t.Errorf("link still holds %s after the plan dropped ipv6", a)
		}
	}
	if hasRule(nl, unix.AF_INET6, 102, 0x6b700001, 200) {
		t.Error("the ipv6 divert rule outlived the plan that wanted it")
	}
	if hasRoute(nl, unix.AF_INET6, 200, "fd64:f5ac:e961::") {
		t.Error("the ipv6 return route outlived the plan that wanted it")
	}
	if !hasRule(nl, unix.AF_INET, 102, 0x6b700001, 200) || !hasRoute(nl, unix.AF_INET, 200, "169.254.77.0") {
		t.Errorf("ipv4 return path lost: rules %+v routes %+v", nl.rules, nl.routes)
	}
}

// TestTeardownRemovesIPv6 is the shutdown half: the IPv6 rules and routes go
// with the IPv4 ones.
func TestTeardownRemovesIPv6(t *testing.T) {
	nl := newFakeNL()
	h := Handles{NFT: &fakeNFT{}, NL: nl}
	if err := Apply(context.Background(), Render(dualStackTargetState(t)), h); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := Teardown(context.Background(), h); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(nl.rules) != 0 || len(nl.routes) != 0 || len(nl.links) != 0 {
		t.Errorf("after teardown: rules %+v routes %+v links %d", nl.rules, nl.routes, len(nl.links))
	}
}

// TestRenderOrdersRulesByFamily pins a total order on routing rules: the IPv4
// and IPv6 divert rules share pref, mark and table, and an order that left
// them tied would let two renders of one State differ.
func TestRenderOrdersRulesByFamily(t *testing.T) {
	rules := []IPRule{
		{Pref: 102, Mark: 0x6b700001, Table: 200, Family: FamilyIPv6},
		{Pref: 102, Mark: 0x6b700001, Table: 200},
	}
	got := Render(State{Rules: rules}).IPRules
	if got[0].Family != FamilyIPv4 || got[1].Family != FamilyIPv6 {
		t.Errorf("rules = %+v, want the ipv4 divert before the ipv6 one", got)
	}
}
