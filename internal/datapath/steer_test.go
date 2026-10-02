package datapath

import (
	"net/netip"
	"reflect"
	"testing"
)

// TestSteerV6IsChecksumNeutral checks the property the IPv6 rewrite relies on
// in place of a checksum update: a stand-in sums to the same ones' complement
// value as the address it replaces. 0x0000 and 0xffff are the same value.
func TestSteerV6IsChecksumNeutral(t *testing.T) {
	norm := func(s uint16) uint16 {
		if s == 0xffff {
			return 0
		}
		return s
	}
	for i, s := range []string{
		"2a0a:4cc0:c1:468:74b9:36ff:fec0:cb48",
		"fd8c:7e8f:50bf:1ab4:9921:ffd0:3bb6:6ae7",
		"fd94::1",
		"2001:db8::1",
		"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
		"::1",
	} {
		a := netip.MustParseAddr(s)
		v := steerV6(i+1, a)
		if !SteerV6.Contains(v) {
			t.Errorf("%s: stand-in %s is outside %s", a, v, SteerV6)
		}
		if got, want := norm(onesSum(v.As16())), norm(onesSum(a.As16())); got != want {
			t.Errorf("%s: stand-in %s sums to %#04x, want %#04x", a, v, got, want)
		}
	}
}

// TestSteerPairs pins the allocation: one stand-in per local address in sorted
// order and per family, the return links' ends counted as local, and nothing
// handed to an address inside a steer block.
func TestSteerPairs(t *testing.T) {
	s := State{
		LocalAddrs: []netip.Addr{
			netip.MustParseAddr("152.53.141.215"),
			netip.MustParseAddr("100.64.234.169"),
			netip.MustParseAddr("169.254.76.9"),
			netip.MustParseAddr("100.64.234.169"),
		},
		Links: []Link{{Name: "kup-1", LinkAddr: netip.MustParsePrefix("169.254.77.1/31")}},
	}
	var got []string
	for _, p := range steerPairs(s) {
		got = append(got, p.Local.String()+" "+p.Virtual.String())
	}
	want := []string{
		"100.64.234.169 169.254.76.1",
		"152.53.141.215 169.254.76.2",
		"169.254.77.1 169.254.76.3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pairs = %q, want %q", got, want)
	}
}

// TestRenderSteersEachDNAT checks the rules a mapping gets on a node with local
// addresses: a steer rule beside each DNAT rule, one restore rule per steered
// family, a link DNAT rule pinned to its end's stand-in, and the maps' entries.
func TestRenderSteersEachDNAT(t *testing.T) {
	end := netip.MustParseAddr("169.254.77.1")
	s := State{
		DNAT: []DNATRule{
			{Iface: "ens3", Port: PortSel{"udp", 9987, 9987}, ToAddr: pod},
			{Iface: "kup-1", Port: PortSel{"udp", 9987, 9987}, ToAddr: pod, DstAddr: &end},
		},
		Links:      []Link{{Name: "kup-1", LinkAddr: netip.MustParsePrefix("169.254.77.1/31")}},
		LocalAddrs: []netip.Addr{netip.MustParseAddr("152.53.141.215")},
	}
	p := Render(s)
	var got []string
	for _, r := range p.Rules {
		got = append(got, r.Chain+": "+nftText(r))
	}
	want := []string{
		`kup-raw: meta nfproto ipv4 iifname "ens3" udp dport 9987 ip daddr set ip daddr map @kup-steer4 counter`,
		`kup-raw: meta nfproto ipv4 iifname "kup-1" udp dport 9987 ip daddr set ip daddr map @kup-steer4 counter`,
		`kup-pre: meta nfproto ipv4 iifname "ens3" udp dport 9987 counter dnat ip to 10.244.18.107:9987`,
		`kup-pre: iifname "kup-1" ip daddr 169.254.76.2 udp dport 9987 counter dnat ip to 10.244.18.107:9987`,
		`kup-restore: meta nfproto ipv4 ip saddr set ip saddr map @kup-restore4 counter`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules:\n%s\nwant:\n%s", join(got), join(want))
	}
	if n := len(p.Steer); n != 2 {
		t.Errorf("map entries = %d, want 2 (the uplink and the link end)", n)
	}
}

func join(lines []string) string {
	out := ""
	for _, l := range lines {
		out += "\t" + l + "\n"
	}
	return out
}
