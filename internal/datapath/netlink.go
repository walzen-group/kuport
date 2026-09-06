package datapath

import (
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// linkPrefix is the name prefix every kuport VXLAN link carries, which is how
// the reconcile recognises its own links without touching anyone else's.
const linkPrefix = "kup-"

// LinkMTU returns the MTU a kuport link can carry over an underlay of the given
// MTU. The 50 bytes are the VXLAN overhead. On the cluster this was designed
// against the margin is exactly zero, which is why task 6 reports both numbers.
func LinkMTU(underlay int) int { return underlay - 50 }

// InterfaceMTU reads an interface's MTU through netlink.
func InterfaceMTU(nl NetlinkConn, name string) (int, error) {
	link, err := nl.LinkByName(name)
	if err != nil {
		return 0, err
	}
	return link.Attrs().MTU, nil
}

// vxlanFor builds the netlink Vxlan object for a Link: a point-to-point tunnel
// whose outer header carries the two nodes' own addresses, which the mesh
// accepts, so no key material is involved.
func vxlanFor(l Link) *netlink.Vxlan {
	return &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: l.Name},
		VxlanId:   int(l.VNI),
		SrcAddr:   l.LocalAddr.AsSlice(),
		Group:     l.RemoteAddr.AsSlice(),
		Port:      int(l.Port),
		TTL:       64,
	}
}

// applyLinks reconciles the VXLAN links: create the missing, address and bring
// them up, and remove any kup-* link the plan no longer names.
func applyLinks(nl NetlinkConn, links []Link) error {
	desired := make(map[string]bool, len(links))
	for _, l := range links {
		desired[l.Name] = true

		existing, err := nl.LinkByName(l.Name)
		if err != nil {
			v := vxlanFor(l)
			if err := nl.LinkAdd(v); err != nil {
				return err
			}
			existing = v
		}

		addr, err := netlink.ParseAddr(l.LinkAddr.String())
		if err != nil {
			return err
		}
		have, err := nl.AddrList(existing, unix.AF_INET)
		if err != nil {
			return err
		}
		if !addrPresent(have, addr) {
			if err := nl.AddrAdd(existing, addr); err != nil {
				return err
			}
		}
		if existing.Attrs().Flags&net.FlagUp == 0 {
			if err := nl.LinkSetUp(existing); err != nil {
				return err
			}
		}
	}

	all, err := nl.LinkList()
	if err != nil {
		return err
	}
	for _, l := range all {
		name := l.Attrs().Name
		if strings.HasPrefix(name, linkPrefix) && !desired[name] {
			if err := nl.LinkDel(l); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyRules reconciles the routing rules: add the missing, and remove any rule
// carrying a kuport-owned mark that the plan no longer wants.
func applyRules(nl NetlinkConn, rules []IPRule) error {
	existing, err := nl.RuleList(unix.AF_INET)
	if err != nil {
		return err
	}
	desired := make([]*netlink.Rule, 0, len(rules))
	for _, r := range rules {
		desired = append(desired, toNLRule(r))
	}

	for _, d := range desired {
		if !ruleListed(existing, d) {
			if err := nl.RuleAdd(d); err != nil {
				return err
			}
		}
	}
	for i := range existing {
		e := &existing[i]
		if e.Mark&ownedMarkMask != ownedMarkValue {
			continue
		}
		if !ruleWanted(desired, e) {
			if err := nl.RuleDel(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyRoutes reconciles the per-table default routes. It only touches the
// tables the plan names, so it never disturbs another table's routes.
func applyRoutes(nl NetlinkConn, routes []Route) error {
	byTable := map[uint32][]*netlink.Route{}
	for _, r := range routes {
		nlr, err := toNLRoute(nl, r)
		if err != nil {
			return err
		}
		byTable[r.Table] = append(byTable[r.Table], nlr)
	}

	for table, desired := range byTable {
		filter := &netlink.Route{Table: int(table)}
		existing, err := nl.RouteListFiltered(unix.AF_INET, filter, netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		for _, d := range desired {
			if !routeListed(existing, d) {
				if err := nl.RouteAdd(d); err != nil {
					return err
				}
			}
		}
		for i := range existing {
			e := &existing[i]
			if !routeWanted(desired, e) {
				if err := nl.RouteDel(e); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// teardownNetlink removes kuport's routes, rules and links by name and mark.
func teardownNetlink(nl NetlinkConn) error {
	all, err := nl.LinkList()
	if err != nil {
		return err
	}
	for _, l := range all {
		if !strings.HasPrefix(l.Attrs().Name, linkPrefix) {
			continue
		}
		routes, err := nl.RouteListFiltered(unix.AF_INET,
			&netlink.Route{LinkIndex: l.Attrs().Index}, netlink.RT_FILTER_OIF)
		if err != nil {
			return err
		}
		for i := range routes {
			if err := nl.RouteDel(&routes[i]); err != nil {
				return err
			}
		}
		if err := nl.LinkDel(l); err != nil {
			return err
		}
	}

	rules, err := nl.RuleList(unix.AF_INET)
	if err != nil {
		return err
	}
	for i := range rules {
		if rules[i].Mark&ownedMarkMask == ownedMarkValue {
			if err := nl.RuleDel(&rules[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// toNLRule maps a kuport IPRule to a netlink rule.
func toNLRule(r IPRule) *netlink.Rule {
	nlr := netlink.NewRule()
	nlr.Priority = int(r.Pref)
	nlr.Mark = r.Mark
	nlr.Table = int(r.Table)
	nlr.Family = unix.AF_INET
	if r.To != nil {
		p := *r.To
		nlr.Dst = &net.IPNet{
			IP:   net.IP(p.Addr().AsSlice()),
			Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen()),
		}
	}
	return nlr
}

// toNLRoute maps a kuport Route to a netlink route, resolving the device index.
func toNLRoute(nl NetlinkConn, r Route) (*netlink.Route, error) {
	link, err := nl.LinkByName(r.Dev)
	if err != nil {
		return nil, err
	}
	return &netlink.Route{
		Table:     int(r.Table),
		Gw:        net.IP(r.Via.AsSlice()),
		LinkIndex: link.Attrs().Index,
		Family:    unix.AF_INET,
	}, nil
}

func addrPresent(have []netlink.Addr, want *netlink.Addr) bool {
	for i := range have {
		if have[i].IPNet != nil && want.IPNet != nil && have[i].IPNet.String() == want.IPNet.String() {
			return true
		}
	}
	return false
}

func ruleEqual(a, b *netlink.Rule) bool {
	if a.Priority != b.Priority || a.Mark != b.Mark || a.Table != b.Table {
		return false
	}
	return ipNetEqual(a.Dst, b.Dst)
}

func ruleListed(list []netlink.Rule, want *netlink.Rule) bool {
	for i := range list {
		if ruleEqual(&list[i], want) {
			return true
		}
	}
	return false
}

func ruleWanted(desired []*netlink.Rule, have *netlink.Rule) bool {
	for _, d := range desired {
		if ruleEqual(d, have) {
			return true
		}
	}
	return false
}

func routeEqual(a, b *netlink.Route) bool {
	return a.Table == b.Table && a.LinkIndex == b.LinkIndex && a.Gw.Equal(b.Gw)
}

func routeListed(list []netlink.Route, want *netlink.Route) bool {
	for i := range list {
		if routeEqual(&list[i], want) {
			return true
		}
	}
	return false
}

func routeWanted(desired []*netlink.Route, have *netlink.Route) bool {
	for _, d := range desired {
		if routeEqual(d, have) {
			return true
		}
	}
	return false
}

func ipNetEqual(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.String() == b.String()
}

// realNetlink is the production NetlinkConn, each method a thin call into the
// netlink package.
type realNetlink struct{}

func (realNetlink) LinkByName(name string) (netlink.Link, error) { return netlink.LinkByName(name) }
func (realNetlink) LinkList() ([]netlink.Link, error)            { return netlink.LinkList() }
func (realNetlink) LinkAdd(link netlink.Link) error              { return netlink.LinkAdd(link) }
func (realNetlink) LinkDel(link netlink.Link) error              { return netlink.LinkDel(link) }
func (realNetlink) LinkSetUp(link netlink.Link) error            { return netlink.LinkSetUp(link) }

func (realNetlink) AddrAdd(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrAdd(link, addr)
}

func (realNetlink) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	return netlink.AddrList(link, family)
}

func (realNetlink) RuleAdd(rule *netlink.Rule) error            { return netlink.RuleAdd(rule) }
func (realNetlink) RuleDel(rule *netlink.Rule) error            { return netlink.RuleDel(rule) }
func (realNetlink) RuleList(family int) ([]netlink.Rule, error) { return netlink.RuleList(family) }
func (realNetlink) RouteAdd(route *netlink.Route) error         { return netlink.RouteAdd(route) }
func (realNetlink) RouteDel(route *netlink.Route) error         { return netlink.RouteDel(route) }

func (realNetlink) RouteListFiltered(family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
	return netlink.RouteListFiltered(family, filter, mask)
}
