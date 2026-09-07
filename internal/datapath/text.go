package datapath

import (
	"fmt"
	"strconv"
	"strings"
)

// String renders the Plan's nftables ruleset as text, byte-for-byte what the
// goldens compare and what nft -c parses. Only the nftables objects appear
// here; the netlink links, rules and routes are asserted against the Plan
// struct directly, since nft cannot parse them.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "table ip %s {\n", TableName)
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

// nftText renders one Rule as an nftables rule line. It is the text sibling of
// nftExprs and must describe the same match and statement.
func nftText(r Rule) string {
	var b strings.Builder

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
		fmt.Fprintf(&b, "ip saddr %s ", r.SrcAddr.String())
	}
	if r.DstAddr != nil {
		fmt.Fprintf(&b, "ip daddr %s ", r.DstAddr.String())
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
		fmt.Fprintf(&b, "counter dnat to %s:%s", r.ToAddr.String(), portText(r.Port))
	case KindSNAT:
		b.WriteString("counter snat to ip saddr")
	case KindMark:
		fmt.Fprintf(&b, "counter meta mark set 0x%x", r.Mark)
	case KindCtSave:
		fmt.Fprintf(&b, "counter ct mark set 0x%x", r.Mark)
	case KindCtLoad:
		b.WriteString("counter meta mark set ct mark")
	}
	return b.String()
}

// portText renders a single port or an inclusive range.
func portText(p PortSel) string {
	if p.First == p.Last {
		return strconv.Itoa(int(p.First))
	}
	return fmt.Sprintf("%d-%d", p.First, p.Last)
}
