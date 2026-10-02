package datapath

import (
	"net/netip"
	"sort"
)

// Fixed object names. The chains are prefixed kup- because nftables rejects the
// bare words mark and fwd as chain names; keeping every name prefixed stays
// clear of the grammar entirely.
const (
	TableName   = "kuport"
	ChainSteer  = "kup-steer"
	ChainPre    = "kup-pre"
	ChainPost   = "kup-post"
	ChainMangle = "kup-mangle"
)

// steerMap names the map kup-steer looks a family's local addresses up in.
func steerMap(f Family) string {
	if f == FamilyIPv6 {
		return "kup-steer6"
	}
	return "kup-steer4"
}

// ownedMarkMask and ownedMarkValue identify the marks kuport sets. Every rule
// kuport owns carries a 0x6b70**** fwmark ("kp" in the high half), which is how
// the netlink reconcile tells its own routing rules apart from anyone else's.
const (
	ownedMarkMask  uint32 = 0xffff0000
	ownedMarkValue uint32 = 0x6b700000
)

// RuleKind is the statement a Rule carries after its matches.
type RuleKind int

const (
	// KindDNAT rewrites the destination to a pod address and port range.
	KindDNAT RuleKind = iota
	// KindSNAT is an identity source NAT (snat to ip saddr) that claims the
	// connection before the CNI's masquerade can.
	KindSNAT
	// KindMark sets an fwmark so replies route back through the accepting node.
	KindMark
	// KindCtSave writes a peer's mark into the flow's conntrack entry, on the
	// request. It sets ct mark alone, leaving the packet's own mark clear, so
	// the request is not caught by the divert rule and sent back out the link
	// it arrived on.
	KindCtSave
	// KindCtLoad restores that mark onto the reply, so it leaves by the link
	// its request arrived on. Under Multi serving it replaces KindMark, which
	// carries one peer's mark and so cannot tell several accepting nodes apart.
	KindCtLoad
	// KindSteer rewrites a mapped request's destination from a local address
	// to its stand-in, through the family's steer map. It runs after
	// conntrack has recorded the address the client dialed, so conntrack
	// writes that address back onto the reply. A destination missing from the
	// map is left alone.
	KindSteer
)

// Rule is one nftables rule described by semantic fields rather than by
// expressions or text. nftExprs (what is applied) and nftText (what the goldens
// compare) both derive from this one struct, so a golden that passes while the
// applied rule is wrong cannot happen. Its family is read off the addresses it
// carries, so an IPv6 rule is the same struct holding IPv6 addresses.
type Rule struct {
	Chain     string
	Kind      RuleKind
	IifName   string      // iifname match; empty means no interface match
	OifName   string      // oifname match; empty means no interface match
	OifNeg    bool        // render oifname != <OifName>; also the cilium_* wildcard
	SrcAddr   *netip.Addr // ip saddr match; nil means none
	DstAddr   *netip.Addr // ip daddr match; nil means none
	Port      PortSel     // matched as dport, or sport when PortIsSrc
	PortIsSrc bool        // match source port instead of destination
	ToAddr    netip.Addr  // KindDNAT target address
	ToPort    uint16      // KindDNAT target port; 0 keeps the matched port
	Mark      uint32      // KindMark value
	MapFamily Family      // KindSteer: whose map to look up
}

// family is the address family a rule is written for. ok is false for a rule
// that carries no address, the conntrack save rule, which matches the link
// alone and so covers a request of either family.
func (r Rule) family() (f Family, ok bool) {
	switch {
	case r.Kind == KindSteer:
		return r.MapFamily, true
	case r.SrcAddr != nil:
		return familyOf(*r.SrcAddr), true
	case r.DstAddr != nil:
		return familyOf(*r.DstAddr), true
	case r.Kind == KindDNAT && r.ToAddr.IsValid():
		return familyOf(r.ToAddr), true
	}
	return FamilyIPv4, false
}

func familyOf(a netip.Addr) Family {
	if a.Unmap().Is4() {
		return FamilyIPv4
	}
	return FamilyIPv6
}

// Plan is the rendered, ordered desired state Apply writes: the nftables rules
// for the four kuport chains and the netlink objects. It is produced purely
// from a State and never touches the host itself.
type Plan struct {
	Rules   []Rule  // ordered across the four chains
	Steer   []Steer // the steer maps' entries, for steered families
	Links   []Link
	IPRules []IPRule
	Routes  []Route
}

// chainDef pins a chain's name and its nftables base-chain header line.
type chainDef struct {
	name   string
	header string
}

// chains are rendered in this fixed order. kup-steer is a filter chain
// because a filter chain runs at the priority it declares, which a nat chain
// does not: the kernel calls every nat chain from the one hook it registers at
// the dstnat priority, so kup-pre's own priority only orders it among nat
// chains. -120 puts kup-steer after conntrack (-200) and a mesh's mark rules
// at mangle (-150), and before a host firewall's filter at -110.
var chains = []chainDef{
	{ChainSteer, "type filter hook prerouting priority dstnat - 20; policy accept;"},
	{ChainPre, "type nat hook prerouting priority dstnat - 20; policy accept;"},
	{ChainPost, "type nat hook postrouting priority srcnat - 10; policy accept;"},
	{ChainMangle, "type filter hook prerouting priority mangle + 10; policy accept;"},
}

var chainOrder = map[string]int{ChainSteer: 0, ChainPre: 1, ChainPost: 2, ChainMangle: 3}

// Render turns a desired State into an ordered Plan. It is deterministic: two
// calls on the same State, whatever the map iteration order upstream, produce a
// byte-identical Plan and thus a byte-identical ruleset.
func Render(s State) Plan {
	var rules []Rule

	pairs := steerPairs(s)
	stand := map[netip.Addr]netip.Addr{}
	hasPairs := map[Family]bool{}
	for _, p := range pairs {
		stand[p.Local] = p.Virtual
		hasPairs[familyOf(p.Local)] = true
	}

	// Each DNAT rule gets a steer rule on the same interface and port. A rule
	// pinned to a local address, a return link's end, is pinned to that
	// address's stand-in instead, since kup-steer has rewritten it by the time
	// kup-pre runs.
	steered := map[Family]bool{}
	steerSeen := map[string]bool{}
	for _, d := range s.DNAT {
		dst := d.DstAddr
		if dst != nil {
			if v, ok := stand[dst.Unmap()]; ok {
				dst = &v
			}
		}
		rules = append(rules, Rule{
			Chain:   ChainPre,
			Kind:    KindDNAT,
			IifName: d.Iface,
			DstAddr: dst,
			Port:    d.Port,
			ToAddr:  d.ToAddr,
			ToPort:  d.ToPort,
		})

		fam := familyOf(d.ToAddr)
		key := d.Iface + "|" + selText(d.Port) + "|" + nfprotoText(fam)
		if !hasPairs[fam] || steerSeen[key] {
			continue
		}
		steerSeen[key] = true
		steered[fam] = true
		rules = append(rules, Rule{
			Chain:     ChainSteer,
			Kind:      KindSteer,
			IifName:   d.Iface,
			Port:      d.Port,
			MapFamily: fam,
		})
	}
	var steer []Steer
	for _, p := range pairs {
		if steered[familyOf(p.Local)] {
			steer = append(steer, p)
		}
	}
	for _, e := range s.Exempt {
		rules = append(rules, Rule{
			Chain:     ChainPost,
			Kind:      KindSNAT,
			OifName:   e.OifName,
			OifNeg:    e.Negate,
			SrcAddr:   e.SrcAddr,
			DstAddr:   e.DstAddr,
			Port:      e.Port,
			PortIsSrc: e.PortIsSrc,
		})
	}
	for _, m := range s.Mark {
		src := m.SrcAddr
		rules = append(rules, Rule{
			Chain:     ChainMangle,
			Kind:      KindMark,
			SrcAddr:   &src,
			Port:      m.Port,
			PortIsSrc: true,
			Mark:      m.Mark,
		})
	}
	// Save and load rules match disjointly and neither issues a verdict, so
	// their order in the chain does not matter. A request arriving on a link
	// carries the client's source address, so it never matches the load rule;
	// a reply from the pod arrives on its veth, so it never matches a save
	// rule. sortRules puts them in interface order like everything else.
	for _, c := range s.CtSave {
		rules = append(rules, Rule{
			Chain:   ChainMangle,
			Kind:    KindCtSave,
			IifName: c.Iface,
			Mark:    c.Mark,
		})
	}
	for _, c := range s.CtLoad {
		src := c.SrcAddr
		rules = append(rules, Rule{
			Chain:     ChainMangle,
			Kind:      KindCtLoad,
			SrcAddr:   &src,
			Port:      c.Port,
			PortIsSrc: true,
		})
	}

	sortRules(rules)

	links := append([]Link(nil), s.Links...)
	sort.Slice(links, func(i, j int) bool { return links[i].Name < links[j].Name })

	iprules := append([]IPRule(nil), s.Rules...)
	sort.Slice(iprules, func(i, j int) bool {
		a, b := iprules[i], iprules[j]
		if a.Pref != b.Pref {
			return a.Pref < b.Pref
		}
		if a.Mark != b.Mark {
			return a.Mark < b.Mark
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		return a.Family < b.Family
	})

	routes := append([]Route(nil), s.Routes...)
	sort.Slice(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.Dev != b.Dev {
			return a.Dev < b.Dev
		}
		return a.Via.String() < b.Via.String()
	})

	return Plan{Rules: rules, Steer: steer, Links: links, IPRules: iprules, Routes: routes}
}

// selText is a port selection as a sort and dedupe key.
func selText(p PortSel) string {
	return p.Proto + "/" + portText(p)
}

// sortRules orders rules by chain, then within a chain by
// (interface, protocol, first port, family, destination address), so the
// output is stable regardless of how the State's slices were built. Family
// sits before the address because a DNAT rule on an accepting interface
// matches no address, and its IPv4 and IPv6 rules would otherwise tie.
func sortRules(rules []Rule) {
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if chainOrder[a.Chain] != chainOrder[b.Chain] {
			return chainOrder[a.Chain] < chainOrder[b.Chain]
		}
		if ai, bi := ifaceKey(a), ifaceKey(b); ai != bi {
			return ai < bi
		}
		if a.Port.Proto != b.Port.Proto {
			return a.Port.Proto < b.Port.Proto
		}
		if a.Port.First != b.Port.First {
			return a.Port.First < b.Port.First
		}
		if af, bf := familyKey(a), familyKey(b); af != bf {
			return af < bf
		}
		return dstKey(a) < dstKey(b)
	})
}

// familyKey sorts a rule with no family first, then IPv4, then IPv6.
func familyKey(r Rule) int {
	f, ok := r.family()
	if !ok {
		return 0
	}
	return int(f) + 1
}

func ifaceKey(r Rule) string {
	if r.IifName != "" {
		return r.IifName
	}
	return r.OifName
}

func dstKey(r Rule) string {
	if r.DstAddr != nil {
		return r.DstAddr.String()
	}
	if r.SrcAddr != nil {
		return r.SrcAddr.String()
	}
	return ""
}
