package datapath

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// netnsMarker is logged by the inner run the moment it is inside the fresh
// network namespace. It lets the outer run tell "this environment cannot open
// a netns" (skip) apart from "the update failed on the real kernel" (fail).
const netnsMarker = "KUPT-NS-STARTED"

// TestApplyLinksUpdatesUnderlayOnRealKernel is the F7 end-to-end proof: it
// re-executes itself under `unshare -rn` (the repo's netns harness pattern)
// and runs applyLinks against the real kernel — create, then move the local
// address out from under the existing link and show one apply reconciles the
// underlay in place: same index, the /31 intact, the link never taken down,
// nothing deleted. It then changes the remaining params one at a time on the
// live interface, which also answers whether the kernel accepts each change
// while the device is up. Where the kernel cannot open a netns the test
// skips.
func TestApplyLinksUpdatesUnderlayOnRealKernel(t *testing.T) {
	inNetns(t, "TestApplyLinksUpdatesUnderlayOnRealKernel", applyLinksRealKernel)
}

// inNetns runs body inside a fresh network namespace by re-executing the test
// binary under `unshare -rn`, filtered to the named test. The outer run skips
// where the kernel cannot open a netns and fails when the inner run does.
func inNetns(t *testing.T, name string, body func(*testing.T)) {
	t.Helper()
	if os.Getenv("KUPORT_NETNS_TEST") == "1" {
		t.Log(netnsMarker)
		body(t)
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not on PATH; real-kernel proof skipped")
	}
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("cannot open a fresh netns here: %v\n%s", err, out)
	}

	cmd := exec.Command("unshare", "-rn", os.Args[0], "-test.run=^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), "KUPORT_NETNS_TEST=1")
	out, err := cmd.CombinedOutput()
	if !bytes.Contains(out, []byte(netnsMarker)) {
		t.Skipf("re-exec under unshare never reached the netns body: %v\n%s", err, out)
	}
	if err != nil {
		t.Fatalf("real-kernel run failed: %v\n%s", err, out)
	}
	t.Logf("real-kernel run:\n%s", out)
}

// TestApplyIPv6OnRealKernel applies the dual-stack target plan to a real
// kernel in a fresh netns, nftables included: the kernel validates every
// expression of the inet ruleset, the /127 lands on the link, and the IPv6
// divert rule and return route exist beside the IPv4 ones. A table ip kuport
// left by an older release is gone after the first pass. An IPv4-only plan
// then removes every IPv6 object, and Teardown removes the rest.
func TestApplyIPv6OnRealKernel(t *testing.T) {
	inNetns(t, "TestApplyIPv6OnRealKernel", applyIPv6RealKernel)
}

func applyIPv6RealKernel(t *testing.T) {
	nl := realNetlink{}
	lo, err := nl.LinkByName("lo")
	if err != nil {
		t.Fatalf("lo: %v", err)
	}
	if err := nl.LinkSetUp(lo); err != nil {
		t.Fatalf("lo up: %v", err)
	}
	local, err := netlink.ParseAddr("100.64.0.2/32")
	if err != nil {
		t.Fatal(err)
	}
	if err := nl.AddrAdd(lo, local); err != nil {
		t.Fatalf("addr add: %v", err)
	}

	conn, err := nftables.New()
	if err != nil {
		t.Fatalf("nftables: %v", err)
	}
	// The table an older release wrote.
	conn.AddTable(legacyTable())
	if err := conn.Flush(); err != nil {
		t.Fatalf("seed table ip kuport: %v", err)
	}
	h := Handles{NFT: conn, NL: nl}

	if err := Apply(context.Background(), Render(dualStackTargetState(t)), h); err != nil {
		t.Fatalf("dual-stack apply: %v", err)
	}
	if tables := kuportTables(t, conn); !reflect.DeepEqual(tables, []string{"inet"}) {
		t.Errorf("kuport tables after apply = %v, want [inet]: the ip table is migrated away", tables)
	}
	if n := inetRuleCount(t, conn); n != 4 {
		t.Errorf("rules in table inet kuport = %d, want 4 (two exemptions, two marks)", n)
	}

	link, err := nl.LinkByName("kup-da0d9a1d")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if !kernelHasAddr(t, nl, link, "fd64:f5ac:e961::1/127") {
		t.Error("the /127 is not on the link")
	}
	if !kernelHasRule(t, nl, unix.AF_INET6, 102, 0x6b700001, 200) {
		t.Error("no ipv6 divert rule at pref 102")
	}
	if !kernelHasRoute(t, nl, unix.AF_INET6, 200, "fd64:f5ac:e961::") {
		t.Error("no ipv6 default route in table 200")
	}

	if err := Apply(context.Background(), Render(targetRemoteState(t)), h); err != nil {
		t.Fatalf("ipv4 apply: %v", err)
	}
	if kernelHasAddr(t, nl, link, "fd64:f5ac:e961::1/127") {
		t.Error("the /127 outlived the plan that wanted it")
	}
	if kernelHasRule(t, nl, unix.AF_INET6, 102, 0x6b700001, 200) {
		t.Error("the ipv6 divert rule outlived the plan that wanted it")
	}
	if kernelHasRoute(t, nl, unix.AF_INET6, 200, "fd64:f5ac:e961::") {
		t.Error("the ipv6 return route outlived the plan that wanted it")
	}
	if !kernelHasRule(t, nl, unix.AF_INET, 102, 0x6b700001, 200) {
		t.Error("the ipv4 divert rule went with the ipv6 one")
	}

	// Every golden case through the kernel, which rejects an expression whose
	// register width, offset or NAT family does not fit: the DNAT rules in
	// both families, the link DNAT, the conntrack save and restore. The links
	// stay out: the multi case gives two of them one VNI, which the golden
	// does not care about and the kernel refuses.
	for _, c := range cases() {
		s := c.state
		plan := Render(State{DNAT: s.DNAT, Exempt: s.Exempt, Mark: s.Mark, CtSave: s.CtSave, CtLoad: s.CtLoad})
		if err := Apply(context.Background(), plan, h); err != nil {
			t.Errorf("%s: apply: %v", c.name, err)
			continue
		}
		if n := inetRuleCount(t, conn); n != len(plan.Rules) {
			t.Errorf("%s: kernel holds %d rules, plan has %d", c.name, n, len(plan.Rules))
		}
	}

	if err := Apply(context.Background(), Render(dualStackTargetState(t)), h); err != nil {
		t.Fatalf("second dual-stack apply: %v", err)
	}
	if err := Teardown(context.Background(), h); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if tables := kuportTables(t, conn); len(tables) != 0 {
		t.Errorf("kuport tables after teardown = %v, want none", tables)
	}
	for _, fam := range []int{unix.AF_INET, unix.AF_INET6} {
		rules, err := nl.RuleList(fam)
		if err != nil {
			t.Fatalf("rule list: %v", err)
		}
		for _, r := range rules {
			if r.Mark&ownedMarkMask == ownedMarkValue {
				t.Errorf("family %d rule %+v outlived teardown", fam, r)
			}
		}
	}
}

// TestApplyMovesChainPriorityOnRealKernel seeds table inet kuport the way
// v0.6.1 left it, with kup-pre at dstnat - 10, and shows one apply moves the
// chain to dstnat - 20, ahead of a host firewall's prerouting filter at
// dstnat - 10. The kernel refuses to change a live base chain's priority in
// place, so this only passes when the apply replaces the table.
func TestApplyMovesChainPriorityOnRealKernel(t *testing.T) {
	inNetns(t, "TestApplyMovesChainPriorityOnRealKernel", applyMovesChainPriorityRealKernel)
}

func applyMovesChainPriorityRealKernel(t *testing.T) {
	conn, err := nftables.New()
	if err != nil {
		t.Fatalf("nftables: %v", err)
	}
	accept := nftables.ChainPolicyAccept
	old := nftTable()
	conn.AddTable(old)
	conn.AddChain(&nftables.Chain{
		Name:     ChainPre,
		Table:    old,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATDest - 10),
		Policy:   &accept,
	})
	if err := conn.Flush(); err != nil {
		t.Fatalf("seed the v0.6.1 table: %v", err)
	}

	h := Handles{NFT: conn, NL: realNetlink{}}
	plan := Render(State{DNAT: targetRemoteState(t).DNAT})
	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("apply over the v0.6.1 table: %v", err)
	}

	chains, err := conn.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatalf("list chains: %v", err)
	}
	for _, c := range chains {
		if c.Table.Name != TableName || c.Name != ChainPre {
			continue
		}
		if want := *nftables.ChainPriorityNATDest - 20; *c.Priority != want {
			t.Errorf("%s priority = %d, want %d", ChainPre, *c.Priority, want)
		}
		return
	}
	t.Fatalf("no chain %s in table inet %s after apply", ChainPre, TableName)
}

// kuportTables names the families that hold a table called kuport.
func kuportTables(t *testing.T, conn *nftables.Conn) []string {
	t.Helper()
	tables, err := conn.ListTables()
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var out []string
	for _, tb := range tables {
		if tb.Name != TableName {
			continue
		}
		switch tb.Family {
		case nftables.TableFamilyINet:
			out = append(out, "inet")
		case nftables.TableFamilyIPv4:
			out = append(out, "ip")
		default:
			out = append(out, "other")
		}
	}
	return out
}

func inetRuleCount(t *testing.T, conn *nftables.Conn) int {
	t.Helper()
	n := 0
	for _, c := range nftChains(nftTable()) {
		rules, err := conn.GetRules(nftTable(), c)
		if err != nil {
			t.Fatalf("rules of %s: %v", c.Name, err)
		}
		n += len(rules)
	}
	return n
}

func kernelHasAddr(t *testing.T, nl NetlinkConn, link netlink.Link, cidr string) bool {
	t.Helper()
	addrs, err := nl.AddrList(link, unix.AF_INET6)
	if err != nil {
		t.Fatalf("addr list: %v", err)
	}
	for _, a := range addrs {
		if a.IPNet.String() == cidr {
			return true
		}
	}
	return false
}

func kernelHasRule(t *testing.T, nl NetlinkConn, family, pref int, mark uint32, table int) bool {
	t.Helper()
	rules, err := nl.RuleList(family)
	if err != nil {
		t.Fatalf("rule list: %v", err)
	}
	for _, r := range rules {
		if r.Priority == pref && r.Mark == mark && r.Table == table {
			return true
		}
	}
	return false
}

func kernelHasRoute(t *testing.T, nl NetlinkConn, family, table int, via string) bool {
	t.Helper()
	routes, err := nl.RouteListFiltered(family, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatalf("route list: %v", err)
	}
	for _, r := range routes {
		if r.Gw.Equal(net.ParseIP(via)) {
			return true
		}
	}
	return false
}

// liveUnderlay reads back the kernel's view of the link and re-pins the
// invariants the F7 update must never break: the device index is the one
// captured at creation (idx, >0), the link is still up, the /31 is still on
// it, and the host still holds exactly one kup-* device.
func liveUnderlay(t *testing.T, nl NetlinkConn, want Link, idx int) *netlink.Vxlan {
	t.Helper()
	l, err := nl.LinkByName(want.Name)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", want.Name, err)
	}
	v, ok := l.(*netlink.Vxlan)
	if !ok {
		t.Fatalf("%s is %T on the real kernel, want *netlink.Vxlan", want.Name, l)
	}
	if idx > 0 && v.Attrs().Index != idx {
		t.Fatalf("device index moved %d -> %d: the update must not recreate the link", idx, v.Attrs().Index)
	}
	if v.Attrs().Flags&net.FlagUp == 0 {
		t.Errorf("%s is down after the update; the live interface must stay up", want.Name)
	}
	addrs, err := nl.AddrList(v, 0) // 0 = AF_UNSPEC: dump every family, the v4 is what we hold
	if err != nil {
		t.Fatalf("AddrList: %v", err)
	}
	addr, err := netlink.ParseAddr(want.LinkAddr.String())
	if err != nil {
		t.Fatalf("parse link addr: %v", err)
	}
	if !addrPresent(addrs, addr) {
		t.Errorf("%s lost its /31 during the update: have %v", want.Name, addrs)
	}
	all, err := nl.LinkList()
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	var n int
	for _, other := range all {
		if strings.HasPrefix(other.Attrs().Name, linkPrefix) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("kup-* devices on host = %d, want 1 (no deletes, no duplicates)", n)
	}
	return v
}

func applyLinksRealKernel(t *testing.T) {
	nl := realNetlink{}

	// A fresh netns holds only a down lo, and a VXLAN can bind only a local
	// address the host owns: give it the candidates and bring lo up.
	lo, err := nl.LinkByName("lo")
	if err != nil {
		t.Fatalf("lo: %v", err)
	}
	if err := nl.LinkSetUp(lo); err != nil {
		t.Fatalf("lo up: %v", err)
	}
	for _, cidr := range []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"} {
		a, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("parse %s: %v", cidr, err)
		}
		if err := nl.AddrAdd(lo, a); err != nil {
			t.Fatalf("addr add %s: %v", cidr, err)
		}
	}

	desired := Link{
		Name:       "kup-kern0000",
		VNI:        4242,
		Port:       4790,
		LocalAddr:  netip.MustParseAddr("10.0.0.1"),
		RemoteAddr: netip.MustParseAddr("10.0.0.9"),
		LinkAddr:   netip.MustParsePrefix("169.254.77.1/31"),
	}
	if err := applyLinks(nl, []Link{desired}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	created := liveUnderlay(t, nl, desired, 0)
	if !created.SrcAddr.Equal(desired.LocalAddr.AsSlice()) {
		t.Fatalf("created link has local %v, want %v", created.SrcAddr, desired.LocalAddr)
	}
	idx := created.Attrs().Index

	// The F7 event: the node's InternalIP moved. One apply, no delete.
	desired.LocalAddr = netip.MustParseAddr("10.0.0.2")
	if err := applyLinks(nl, []Link{desired}); err != nil {
		t.Fatalf("apply after address change: %v", err)
	}
	updated := liveUnderlay(t, nl, desired, idx)
	if !updated.SrcAddr.Equal(desired.LocalAddr.AsSlice()) {
		t.Errorf("after address change the kernel holds local %v, want %v", updated.SrcAddr, desired.LocalAddr)
	}
	// A re-read of the kernel's own dump must satisfy the decision as a
	// keep: this is what keeps the next apply a no-op, not an update loop.
	if got := linkActionFor(updated, desired); got != linkKeep {
		t.Errorf("kernel dump of the link we just wrote re-reads as action %d, want keep", got)
	}

	// The rest of the params, one at a time, on the live interface.
	// retarget=true pins the kernel's live in-place change; identity
	// params (vni, port) pin the rebuild path: the name is kept, the
	// device index moves, and the same apply restores the /31.
	steps := []struct {
		name     string
		mutate   func()
		retarget bool
	}{
		{"remote", func() { desired.RemoteAddr = netip.MustParseAddr("10.0.0.10") }, true},
		{"vni", func() { desired.VNI = 7777 }, false},
		{"port", func() { desired.Port = 12345 }, false},
		{"local again", func() { desired.LocalAddr = netip.MustParseAddr("10.0.0.3") }, true},
	}
	for _, s := range steps {
		before := idx
		s.mutate()
		if err := applyLinks(nl, []Link{desired}); err != nil {
			t.Errorf("apply after %s change: %v", s.name, err)
			continue
		}
		v := liveUnderlay(t, nl, desired, 0)
		if s.retarget && v.Attrs().Index != before {
			t.Errorf("after %s change the index moved %d -> %d: endpoints must retarget in place",
				s.name, before, v.Attrs().Index)
		}
		if !s.retarget && v.Attrs().Index == before {
			t.Errorf("after %s change the index held at %d: an identity change must rebuild", s.name, before)
		}
		idx = v.Attrs().Index
		if !v.SrcAddr.Equal(desired.LocalAddr.AsSlice()) || !v.Group.Equal(desired.RemoteAddr.AsSlice()) ||
			v.VxlanId != int(desired.VNI) || v.Port != int(desired.Port) {
			t.Errorf("after %s change the kernel holds local=%v group=%v vni=%d port=%d, want %v %v %d %d",
				s.name, v.SrcAddr, v.Group, v.VxlanId, v.Port,
				desired.LocalAddr, desired.RemoteAddr, desired.VNI, desired.Port)
		}
		if got := linkActionFor(v, desired); got != linkKeep {
			t.Errorf("after %s change the fresh dump re-reads as action %d, want keep", s.name, got)
		}
	}
}
