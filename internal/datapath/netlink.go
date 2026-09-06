package datapath

import (
	"net"
	"net/netip"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
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

// underlay is the effective VXLAN underlay read off an existing link: the
// four params that decide where tunnel traffic actually goes.
type underlay struct {
	local  net.IP
	remote net.IP
	vni    int
	port   int
}

// underlayOf extracts a link's effective underlay: the remote end is carried
// in Group, which is where vxlanFor puts it and where a kernel dump of a
// unicast-remote VXLAN reads back. It reports false for a non-VXLAN link:
// with no underlay to read there is nothing to compare, and kuport must not
// guess about a device it does not recognise.
func underlayOf(l netlink.Link) (underlay, bool) {
	v, ok := l.(*netlink.Vxlan)
	if !ok {
		return underlay{}, false
	}
	return underlay{local: v.SrcAddr, remote: v.Group, vni: v.VxlanId, port: v.Port}, true
}

// linkAction is what applyLinks must do with an existing link of the right
// name. The split mirrors what the kernel actually accepts on a live device
// (probed on 6.18, and read in drivers/net/vxlan): the two endpoint
// addresses can be re-sent on a running VXLAN, while VNI and destination
// port are identity params it refuses to change at all — those need the
// device rebuilt, which the same apply pass re-addresses and re-routes.
type linkAction int

const (
	linkKeep linkAction = iota
	linkRetarget
	linkRebuild
)

// linkActionFor is the F7 decision: an existing link is kept only when every
// underlay param equals the desired one. A moved InternalIP or moved peer
// retargets the device in place — the /31 and its routes survive, live
// traffic does not drop, the index never moves. A re-keyed VNI, a remapped
// port, or an underlay that cannot be read at all (not a VXLAN, a
// family-flipped endpoint, a device that was never bound) rebuilds it.
func linkActionFor(l netlink.Link, want Link) linkAction {
	have, ok := underlayOf(l)
	if !ok {
		return linkRebuild
	}
	la, lok := unmap(have.local)
	ra, rok := unmap(have.remote)
	if !lok || !rok {
		return linkRebuild // an underlay we cannot read is not one to keep
	}
	wantLocal, wantRemote := want.LocalAddr.Unmap(), want.RemoteAddr.Unmap()
	if la != wantLocal || ra != wantRemote {
		if la.Is4() != wantLocal.Is4() || ra.Is4() != wantRemote.Is4() {
			return linkRebuild // a family flip rebinds the socket
		}
		return linkRetarget
	}
	if have.vni != int(want.VNI) || have.port != int(want.Port) {
		return linkRebuild
	}
	return linkKeep
}

// unmap normalises a dumped net.IP (4-byte or v4-in-v6) to a comparable
// netip.Addr, and reports absence: an endpoint the kernel does not report is
// not an endpoint that equals the desired one.
func unmap(ip net.IP) (netip.Addr, bool) {
	if len(ip) == 0 {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
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

// applyLinks reconciles the VXLAN links: create the missing, retarget the
// ones whose underlay has gone stale, rebuild the ones whose identity the
// kernel cannot change, address and bring them up, and remove any kup-* link
// the plan no longer names.
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
		} else {
			switch linkActionFor(existing, l) {
			case linkKeep:
			case linkRetarget:
				// F7: the endpoints moved — a changed InternalIP on
				// this node or on the peer. Re-point the existing
				// device in place; deleting it would tear the
				// address and routes off the /31 and drop live
				// traffic, which the level-driven design cannot
				// afford on every reconcile.
				if us, ok := nl.(underlaySetter); ok {
					if err := us.LinkSetUnderlay(existing.Attrs().Index, l.Name,
						l.LocalAddr.AsSlice(), l.RemoteAddr.AsSlice()); err != nil {
						return err
					}
					break
				}
				// A connection without the retarget capability
				// rebuilds instead: the same state, one index move
				// further.
				fallthrough
			case linkRebuild:
				// An identity the kernel cannot change on an
				// existing device (VNI, destination port), or an
				// underlay we cannot read (not a VXLAN, a family
				// flip, an endpoint the dump does not report).
				// Rebuild: this tunnel was not carrying this plan's
				// traffic anyway; the same apply re-adds the /31
				// below and applyRoutes — last in Apply — restates
				// every route the old index dropped.
				if err := nl.LinkDel(existing); err != nil {
					return err
				}
				v := vxlanFor(l)
				if err := nl.LinkAdd(v); err != nil {
					return err
				}
				existing = v
			}
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

// LinkSetUnderlay retargets an existing VXLAN's endpoints with the smallest
// netlink message the kernel accepts on a running device: an RTM_NEWLINK
// naming the device and carrying only the local bind address and the group
// (the peer's unicast address) in its vxlan info — an existing name routes
// the request to the kernel's changelink, which starts from the device's
// current config, so attributes left out keep their values. (RTM_SETLINK
// does not carry link info at all: it succeeds while changing nothing.)
// It deliberately does not use netlink.LinkModify: that path re-sends every
// vxlan attribute at once, and the kernel's changelink rejects the presence
// of the proxy/rsc/l2miss/l3miss flags outright (vxlan_nl2flag in
// drivers/net/vxlan), whatever their values — a whole-object modify is
// therefore guaranteed EOPNOTSUPP.
func (realNetlink) LinkSetUnderlay(idx int, name string, local, remote net.IP) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(idx)
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFLA_IFNAME, nl.NonZeroTerminated(name)))

	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated("vxlan"))
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	if ip := local.To4(); ip != nil {
		data.AddRtAttr(nl.IFLA_VXLAN_LOCAL, ip)
	} else if ip := local.To16(); ip != nil {
		data.AddRtAttr(nl.IFLA_VXLAN_LOCAL6, ip)
	}
	if ip := remote.To4(); ip != nil {
		data.AddRtAttr(nl.IFLA_VXLAN_GROUP, ip)
	} else if ip := remote.To16(); ip != nil {
		data.AddRtAttr(nl.IFLA_VXLAN_GROUP6, ip)
	}
	req.AddData(linkInfo)

	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

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
