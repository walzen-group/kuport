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
	ChainPre    = "kup-pre"
	ChainPost   = "kup-post"
	ChainMangle = "kup-mangle"
)

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
)

// Rule is one nftables rule described by semantic fields rather than by
// expressions or text. nftExprs (what is applied) and nftText (what the goldens
// compare) both derive from this one struct, so a golden that passes while the
// applied rule is wrong cannot happen.
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
	Mark      uint32      // KindMark value
}

// Plan is the rendered, ordered desired state Apply writes: the nftables rules
// for the three kuport chains and the netlink objects. It is produced purely
// from a State and never touches the host itself.
type Plan struct {
	Rules   []Rule // ordered across the three chains
	Links   []Link
	IPRules []IPRule
	Routes  []Route
}

// chainDef pins a chain's name and its nftables base-chain header line.
type chainDef struct {
	name   string
	header string
}

// chains are rendered in this fixed order.
var chains = []chainDef{
	{ChainPre, "type nat hook prerouting priority dstnat - 10; policy accept;"},
	{ChainPost, "type nat hook postrouting priority srcnat - 10; policy accept;"},
	{ChainMangle, "type filter hook prerouting priority mangle + 10; policy accept;"},
}

var chainOrder = map[string]int{ChainPre: 0, ChainPost: 1, ChainMangle: 2}

// Render turns a desired State into an ordered Plan. It is deterministic: two
// calls on the same State, whatever the map iteration order upstream, produce a
// byte-identical Plan and thus a byte-identical ruleset.
func Render(s State) Plan {
	var rules []Rule

	for _, d := range s.DNAT {
		rules = append(rules, Rule{
			Chain:   ChainPre,
			Kind:    KindDNAT,
			IifName: d.Iface,
			DstAddr: d.DstAddr,
			Port:    d.Port,
			ToAddr:  d.ToAddr,
		})
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
		return a.Table < b.Table
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

	return Plan{Rules: rules, Links: links, IPRules: iprules, Routes: routes}
}

// sortRules orders rules by chain, then within a chain by
// (interface, protocol, first port, destination address), so the output is
// stable regardless of how the State's slices were built.
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
		return dstKey(a) < dstKey(b)
	})
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
