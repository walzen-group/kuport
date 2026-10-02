package datapath

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/google/nftables"
)

// String renders the Plan's nftables ruleset as text, byte-for-byte what the
// goldens compare and what nft -c parses. Only the nftables objects appear
// here; the netlink links, rules and routes are asserted against the Plan
// struct directly, since nft cannot parse them.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", TableName)
	for _, m := range nftMaps(nftTable(), p.Steer) {
		typ := "ipv4_addr"
		if m.set.KeyType == nftables.TypeIP6Addr {
			typ = "ipv6_addr"
		}
		fmt.Fprintf(&b, "\tmap %s {\n", m.set.Name)
		fmt.Fprintf(&b, "\t\ttype %s : %s\n", typ, typ)
		var elems []string
		for _, e := range m.elems {
			elems = append(elems, addrText(e.Key)+" : "+addrText(e.Val))
		}
		fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(elems, ", "))
		b.WriteString("\t}\n")
	}
	for _, c := range chains {
		fmt.Fprintf(&b, "\tchain %s {\n", c.name)
		fmt.Fprintf(&b, "\t\t%s\n", c.header)
		for _, r := range p.Rules {
			if r.Chain == c.name {
				fmt.Fprintf(&b, "\t\t%s\n", nftText(r))
			}
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// nftText renders one Rule as an nftables rule line, the way nft lists it in an
// inet table. It is the text sibling of nftExprs and must describe the same
// match and statement.
//
// nftExprs puts a meta nfproto match first on every rule with a family. nft
// prints it only where nothing later in the rule implies it: an `ip saddr` or
// `ip6 daddr` match already names the family, and nft drops the nfproto match
// in front of one when it lists the rule. The text does the same, so the
// nfproto match shows on the accepting DNAT rules, which match no address.
func nftText(r Rule) string {
	var b strings.Builder

	fam, hasFam := r.family()
	if hasFam && r.SrcAddr == nil && r.DstAddr == nil {
		fmt.Fprintf(&b, "meta nfproto %s ", nfprotoText(fam))
	}

	if r.IifName != "" {
		fmt.Fprintf(&b, "iifname \"%s\" ", r.IifName)
	}
	if r.OifName != "" {
		op := ""
		if r.OifNeg {
			op = "!= "
		}
		fmt.Fprintf(&b, "oifname %s\"%s\" ", op, r.OifName)
	}
	if r.SrcAddr != nil {
		fmt.Fprintf(&b, "%s saddr %s ", l3Text(familyOf(*r.SrcAddr)), r.SrcAddr.String())
	}
	if r.DstAddr != nil {
		fmt.Fprintf(&b, "%s daddr %s ", l3Text(familyOf(*r.DstAddr)), r.DstAddr.String())
	}

	if r.Port.Proto != "" {
		sel := "dport"
		if r.PortIsSrc {
			sel = "sport"
		}
		fmt.Fprintf(&b, "%s %s %s ", r.Port.Proto, sel, portText(r.Port))
	}

	switch r.Kind {
	case KindDNAT:
		to := r.Port
		if r.ToPort != 0 {
			to = PortSel{Proto: r.Port.Proto, First: r.ToPort, Last: r.ToPort}
		}
		// nft brackets an IPv6 target, since its colons would otherwise run
		// into the port's.
		addr := r.ToAddr.String()
		if fam == FamilyIPv6 {
			addr = "[" + addr + "]"
		}
		fmt.Fprintf(&b, "counter dnat %s to %s:%s", l3Text(fam), addr, portText(to))
	case KindSNAT:
		fmt.Fprintf(&b, "counter snat %s to %s saddr", l3Text(fam), l3Text(fam))
	case KindMark:
		fmt.Fprintf(&b, "counter meta mark set 0x%x", r.Mark)
	case KindCtSave:
		fmt.Fprintf(&b, "counter ct mark set 0x%x", r.Mark)
	case KindCtLoad:
		b.WriteString("counter meta mark set ct mark")
	case KindSteer:
		fmt.Fprintf(&b, "%s daddr set %s daddr map @%s counter", l3Text(fam), l3Text(fam), steerMap(fam))
	}
	return b.String()
}

// addrText renders an address held as 4 or 16 network-order bytes.
func addrText(b []byte) string {
	a, _ := netip.AddrFromSlice(b)
	return a.String()
}

// l3Text is the keyword nft uses for a family's header and NAT statement.
func l3Text(f Family) string {
	if f == FamilyIPv6 {
		return "ip6"
	}
	return "ip"
}

// nfprotoText is the value nft prints for a meta nfproto match.
func nfprotoText(f Family) string {
	if f == FamilyIPv6 {
		return "ipv6"
	}
	return "ipv4"
}

// portText renders a single port or an inclusive range.
func portText(p PortSel) string {
	if p.First == p.Last {
		return strconv.Itoa(int(p.First))
	}
	return fmt.Sprintf("%d-%d", p.First, p.Last)
}
