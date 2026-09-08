package datapath

import (
	"context"
	"net"
	"testing"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// fakeNFT records the committed ruleset so a second Apply can be shown to
// produce the identical table. Every pass rewrites the whole table, which is by
// design; idempotence for nftables means the committed result is unchanged.
type fakeNFT struct {
	staged    int
	committed int
	tableAdds int
	tableDels int
	flushes   int
}

func (f *fakeNFT) AddTable(t *nftables.Table) *nftables.Table { f.tableAdds++; return t }
func (f *fakeNFT) FlushTable(t *nftables.Table)               { f.staged = 0 }
func (f *fakeNFT) AddChain(c *nftables.Chain) *nftables.Chain { return c }
func (f *fakeNFT) AddRule(r *nftables.Rule) *nftables.Rule    { f.staged++; return r }
func (f *fakeNFT) DelTable(t *nftables.Table)                 { f.tableDels++ }
func (f *fakeNFT) Flush() error                               { f.committed = f.staged; f.flushes++; return nil }

// fakeNL is a stateful netlink connection: it applies operations to its own
// world so a second Apply finds everything already present and mutates nothing.
// muts counts every mutating call, which is what the idempotence test checks.
type fakeNL struct {
	links   map[string]netlink.Link
	addrs   map[string][]netlink.Addr
	rules   []netlink.Rule
	routes  []netlink.Route
	nextIdx int
	muts    int
}

func newFakeNL() *fakeNL {
	return &fakeNL{links: map[string]netlink.Link{}, addrs: map[string][]netlink.Addr{}, nextIdx: 10}
}

func (f *fakeNL) LinkByName(name string) (netlink.Link, error) {
	if l, ok := f.links[name]; ok {
		return l, nil
	}
	return nil, netlink.LinkNotFoundError{}
}

func (f *fakeNL) LinkList() ([]netlink.Link, error) {
	out := make([]netlink.Link, 0, len(f.links))
	for _, l := range f.links {
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeNL) LinkAdd(link netlink.Link) error {
	f.muts++
	f.nextIdx++
	link.Attrs().Index = f.nextIdx
	f.links[link.Attrs().Name] = link
	return nil
}

func (f *fakeNL) LinkDel(link netlink.Link) error {
	f.muts++
	delete(f.links, link.Attrs().Name)
	return nil
}

func (f *fakeNL) LinkSetUp(link netlink.Link) error {
	f.muts++
	link.Attrs().Flags |= net.FlagUp
	return nil
}

func (f *fakeNL) LinkSetMTU(link netlink.Link, mtu int) error {
	f.muts++
	link.Attrs().MTU = mtu
	return nil
}

// LinkSetUnderlay re-points the stored VXLAN in place: same object, same
// index, endpoints moved — mirroring the kernel's live-retarget path.
func (f *fakeNL) LinkSetUnderlay(idx int, name string, local, remote net.IP) error {
	f.muts++
	for _, l := range f.links {
		v, ok := l.(*netlink.Vxlan)
		if !ok || v.Index != idx {
			continue
		}
		if len(local) > 0 {
			v.SrcAddr = local
		}
		if len(remote) > 0 {
			v.Group = remote
		}
		return nil
	}
	return netlink.LinkNotFoundError{}
}

func (f *fakeNL) AddrAdd(link netlink.Link, addr *netlink.Addr) error {
	f.muts++
	name := link.Attrs().Name
	f.addrs[name] = append(f.addrs[name], *addr)
	return nil
}

func (f *fakeNL) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	return f.addrs[link.Attrs().Name], nil
}

func (f *fakeNL) RuleAdd(rule *netlink.Rule) error {
	f.muts++
	f.rules = append(f.rules, *rule)
	return nil
}

func (f *fakeNL) RuleDel(rule *netlink.Rule) error {
	f.muts++
	for i := range f.rules {
		if f.rules[i].Priority == rule.Priority && f.rules[i].Mark == rule.Mark && f.rules[i].Table == rule.Table {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeNL) RuleList(family int) ([]netlink.Rule, error) {
	return append([]netlink.Rule(nil), f.rules...), nil
}

func (f *fakeNL) RouteAdd(route *netlink.Route) error {
	f.muts++
	f.routes = append(f.routes, *route)
	return nil
}

func (f *fakeNL) RouteDel(route *netlink.Route) error {
	f.muts++
	for i := range f.routes {
		if routeEqual(&f.routes[i], route) {
			f.routes = append(f.routes[:i], f.routes[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeNL) RouteListFiltered(family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
	var out []netlink.Route
	for _, r := range f.routes {
		if mask&netlink.RT_FILTER_TABLE != 0 && r.Table != filter.Table {
			continue
		}
		if mask&netlink.RT_FILTER_OIF != 0 && r.LinkIndex != filter.LinkIndex {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func targetRemoteState(t *testing.T) State {
	t.Helper()
	for _, c := range cases() {
		if c.name == "target-remote" {
			return c.state
		}
	}
	t.Fatal("target-remote case missing")
	return State{}
}

// TestApplyIdempotent applies the same Plan twice against fresh fakes and
// asserts the second pass issues no netlink change and commits the same
// nftables table. A test that applied once would prove nothing.
func TestApplyIdempotent(t *testing.T) {
	plan := Render(targetRemoteState(t))
	nft := &fakeNFT{}
	nl := newFakeNL()
	h := Handles{NFT: nft, NL: nl}

	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	firstMuts := nl.muts
	firstCommitted := nft.committed
	if firstMuts == 0 {
		t.Fatal("first apply issued no netlink mutations; fake is not wired up")
	}

	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if delta := nl.muts - firstMuts; delta != 0 {
		t.Errorf("second apply issued %d netlink mutations, want 0", delta)
	}
	if nft.committed != firstCommitted {
		t.Errorf("second apply committed %d nft rules, first committed %d", nft.committed, firstCommitted)
	}

	// The world holds exactly what the plan asked for.
	if len(nl.links) != 1 || len(nl.rules) != 2 || len(nl.routes) != 1 {
		t.Errorf("world drifted: links=%d rules=%d routes=%d", len(nl.links), len(nl.rules), len(nl.routes))
	}
}

// TestTeardownRemovesOwned tears down after an apply and asserts kuport's links,
// rules and routes are gone and the nft table was deleted.
func TestTeardownRemovesOwned(t *testing.T) {
	plan := Render(targetRemoteState(t))
	nft := &fakeNFT{}
	nl := newFakeNL()
	h := Handles{NFT: nft, NL: nl}

	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := Teardown(context.Background(), h); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(nl.links) != 0 {
		t.Errorf("links remain after teardown: %d", len(nl.links))
	}
	if len(nl.rules) != 0 {
		t.Errorf("rules remain after teardown: %d", len(nl.rules))
	}
	if len(nl.routes) != 0 {
		t.Errorf("routes remain after teardown: %d", len(nl.routes))
	}
	if nft.tableDels != 1 {
		t.Errorf("nft table deletions = %d, want 1", nft.tableDels)
	}
}

// TestApplyReconcilesForeignAwayLeavesOthers checks that a stray kuport-owned
// rule is removed while a rule kuport does not own is left untouched.
func TestApplyReconcilesOwnedOnly(t *testing.T) {
	plan := Render(targetRemoteState(t))
	nft := &fakeNFT{}
	nl := newFakeNL()

	// A stray rule with a kuport mark that the plan does not want.
	nl.rules = append(nl.rules, netlink.Rule{Priority: 199, Mark: 0x6b70ffff, Table: 200, Family: unix.AF_INET})
	// A rule owned by someone else; kuport must not touch it.
	nl.rules = append(nl.rules, netlink.Rule{Priority: 110, Mark: 0x1234, Table: 99, Family: unix.AF_INET})

	h := Handles{NFT: nft, NL: nl}
	if err := Apply(context.Background(), plan, h); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var foundStray, foundForeign bool
	for _, r := range nl.rules {
		if r.Mark == 0x6b70ffff {
			foundStray = true
		}
		if r.Mark == 0x1234 {
			foundForeign = true
		}
	}
	if foundStray {
		t.Error("stray kuport-owned rule was not reconciled away")
	}
	if !foundForeign {
		t.Error("a rule kuport does not own was removed")
	}
}
