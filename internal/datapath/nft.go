package datapath

import (
	"net/netip"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// nftTable is the kuport table object, family ip.
func nftTable() *nftables.Table {
	return &nftables.Table{Name: TableName, Family: nftables.TableFamilyIPv4}
}

// nftChains returns the three base chains in fixed order, hooked and prioritised
// exactly as the golden headers describe: dstnat-10, srcnat-10, mangle+10.
func nftChains(t *nftables.Table) []*nftables.Chain {
	accept := nftables.ChainPolicyAccept
	return []*nftables.Chain{
		{
			Name:     ChainPre,
			Table:    t,
			Type:     nftables.ChainTypeNAT,
			Hooknum:  nftables.ChainHookPrerouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATDest - 10),
			Policy:   &accept,
		},
		{
			Name:     ChainPost,
			Table:    t,
			Type:     nftables.ChainTypeNAT,
			Hooknum:  nftables.ChainHookPostrouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATSource - 10),
			Policy:   &accept,
		},
		{
			Name:     ChainMangle,
			Table:    t,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookPrerouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityMangle + 10),
			Policy:   &accept,
		},
	}
}

// nftExprs builds the netlink expressions that actually get applied for a Rule.
// It is the applied sibling of nftText; both are driven from the same Rule so
// they cannot describe different rules.
func nftExprs(r Rule) []expr.Any {
	var e []expr.Any

	if r.IifName != "" {
		e = append(e,
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			ifnameCmp(r.IifName, false),
		)
	}
	if r.OifName != "" {
		e = append(e,
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			ifnameCmp(r.OifName, r.OifNeg),
		)
	}
	if r.SrcAddr != nil {
		e = append(e, ipCmp(*r.SrcAddr, ipSrcOffset)...)
	}
	if r.DstAddr != nil {
		e = append(e, ipCmp(*r.DstAddr, ipDstOffset)...)
	}

	// Match the L4 protocol, then the port (source or destination).
	e = append(e,
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoNum(r.Port.Proto)}},
	)
	e = append(e, portMatch(r.Port, r.PortIsSrc)...)

	e = append(e, &expr.Counter{})

	switch r.Kind {
	case KindDNAT:
		e = append(e, dnatExprs(r.ToAddr, r.Port)...)
	case KindSNAT:
		e = append(e, snatIdentityExprs()...)
	case KindMark:
		e = append(e, markExprs(r.Mark)...)
	}
	return e
}

// Byte offsets into the IPv4 header for the source and destination address.
const (
	ipSrcOffset uint32 = 12
	ipDstOffset uint32 = 16
)

func protoNum(proto string) byte {
	if proto == "tcp" {
		return unix.IPPROTO_TCP
	}
	return unix.IPPROTO_UDP
}

// ifnameCmp compares the interface name in register 1. A trailing '*' is a
// prefix match, which nftables implements by comparing only the bytes before
// the star; an exact name is compared with its null terminator.
func ifnameCmp(name string, neg bool) *expr.Cmp {
	op := expr.CmpOpEq
	if neg {
		op = expr.CmpOpNeq
	}
	if strings.HasSuffix(name, "*") {
		return &expr.Cmp{Op: op, Register: 1, Data: []byte(strings.TrimSuffix(name, "*"))}
	}
	data := make([]byte, len(name)+1)
	copy(data, name)
	return &expr.Cmp{Op: op, Register: 1, Data: data}
}

// ipCmp loads a 4-byte IPv4 address from the network header at offset and
// compares it for equality.
func ipCmp(a netip.Addr, offset uint32) []expr.Any {
	v4 := a.As4()
	return []expr.Any{
		&expr.Payload{
			OperationType: expr.PayloadLoad,
			DestRegister:  1,
			Base:          expr.PayloadBaseNetworkHeader,
			Offset:        offset,
			Len:           4,
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: v4[:]},
	}
}

// portMatch loads the transport source or destination port and compares it,
// as a single value or an inclusive range.
func portMatch(p PortSel, isSrc bool) []expr.Any {
	offset := uint32(2) // destination port
	if isSrc {
		offset = 0 // source port
	}
	load := &expr.Payload{
		OperationType: expr.PayloadLoad,
		DestRegister:  1,
		Base:          expr.PayloadBaseTransportHeader,
		Offset:        offset,
		Len:           2,
	}
	if p.First == p.Last {
		return []expr.Any{load, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(p.First)}}
	}
	return []expr.Any{load, &expr.Range{
		Op:       expr.CmpOpEq,
		Register: 1,
		FromData: binaryutil.BigEndian.PutUint16(p.First),
		ToData:   binaryutil.BigEndian.PutUint16(p.Last),
	}}
}

// dnatExprs rewrites the destination to a pod address and port range.
func dnatExprs(to netip.Addr, p PortSel) []expr.Any {
	v4 := to.As4()
	out := []expr.Any{
		&expr.Immediate{Register: 1, Data: v4[:]},
		&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(p.First)},
	}
	nat := &expr.NAT{
		Type:        expr.NATTypeDestNAT,
		Family:      unix.NFPROTO_IPV4,
		RegAddrMin:  1,
		RegAddrMax:  1,
		RegProtoMin: 2,
		RegProtoMax: 2,
	}
	if p.First != p.Last {
		out = append(out, &expr.Immediate{Register: 3, Data: binaryutil.BigEndian.PutUint16(p.Last)})
		nat.RegProtoMax = 3
	}
	return append(out, nat)
}

// snatIdentityExprs is the identity source NAT (snat to ip saddr): load the
// packet's own source address and SNAT to it, claiming the connection before
// the CNI's masquerade can rewrite it.
func snatIdentityExprs() []expr.Any {
	return []expr.Any{
		&expr.Payload{
			OperationType: expr.PayloadLoad,
			DestRegister:  1,
			Base:          expr.PayloadBaseNetworkHeader,
			Offset:        ipSrcOffset,
			Len:           4,
		},
		&expr.NAT{
			Type:       expr.NATTypeSourceNAT,
			Family:     unix.NFPROTO_IPV4,
			RegAddrMin: 1,
			RegAddrMax: 1,
		},
	}
}

// markExprs sets the fwmark that steers a reply back to the accepting node.
func markExprs(mark uint32) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	}
}

// applyNFT writes the whole kuport table in one transaction: add the table,
// flush it, add the chains and rules, then flush the connection. Rewriting the
// entire table each pass is what makes a deleted mapping disappear by absence.
func applyNFT(conn NFTConn, plan Plan) error {
	t := nftTable()
	conn.AddTable(t)
	conn.FlushTable(t)

	chainObjs := map[string]*nftables.Chain{}
	for _, c := range nftChains(t) {
		conn.AddChain(c)
		chainObjs[c.Name] = c
	}
	for _, r := range plan.Rules {
		conn.AddRule(&nftables.Rule{
			Table: t,
			Chain: chainObjs[r.Chain],
			Exprs: nftExprs(r),
		})
	}
	return conn.Flush()
}

// teardownNFT removes the whole kuport table.
func teardownNFT(conn NFTConn) error {
	conn.DelTable(nftTable())
	return conn.Flush()
}
