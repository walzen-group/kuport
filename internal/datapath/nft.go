package datapath

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// nftTable is the kuport table object, family inet, so one table holds the
// rules of both address families.
func nftTable() *nftables.Table {
	return &nftables.Table{Name: TableName, Family: nftables.TableFamilyINet}
}

// legacyTable is the family ip table releases before IPv6 wrote. Apply and
// Teardown delete it, so a node upgraded in place does not keep its old DNAT
// rules answering beside the new ones.
func legacyTable() *nftables.Table {
	return &nftables.Table{Name: TableName, Family: nftables.TableFamilyIPv4}
}

// delTable stages a delete that succeeds whether or not the table exists. A
// bare delete of an absent table fails the whole transaction with ENOENT;
// adding it first, which leaves an existing table as it is, gives the delete
// something to remove either way.
func delTable(conn NFTConn, t *nftables.Table) {
	conn.AddTable(t)
	conn.DelTable(t)
}

// nftChains returns the five base chains in fixed order, hooked and prioritised
// exactly as the golden headers describe: raw-10, dstnat-20, srcnat-10,
// mangle+10, srcnat+10.
func nftChains(t *nftables.Table) []*nftables.Chain {
	accept := nftables.ChainPolicyAccept
	return []*nftables.Chain{
		{
			Name:     ChainRaw,
			Table:    t,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookPrerouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityRaw - 10),
			Policy:   &accept,
		},
		{
			Name:     ChainPre,
			Table:    t,
			Type:     nftables.ChainTypeNAT,
			Hooknum:  nftables.ChainHookPrerouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATDest - 20),
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
		{
			Name:     ChainRestore,
			Table:    t,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookPostrouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATSource + 10),
			Policy:   &accept,
		},
	}
}

// nftMaps returns the steer and restore maps of each family the plan steers,
// with their elements: local address to stand-in, and stand-in to local
// address.
func nftMaps(t *nftables.Table, steer []Steer) []nftMap {
	var out []nftMap
	for _, f := range []Family{FamilyIPv4, FamilyIPv6} {
		var fwd, back []nftables.SetElement
		for _, p := range steer {
			if familyOf(p.Local) != f {
				continue
			}
			fwd = append(fwd, nftables.SetElement{Key: addrBytes(p.Local), Val: addrBytes(p.Virtual)})
			back = append(back, nftables.SetElement{Key: addrBytes(p.Virtual), Val: addrBytes(p.Local)})
		}
		if len(fwd) == 0 {
			continue
		}
		typ := nftables.TypeIPAddr
		if f == FamilyIPv6 {
			typ = nftables.TypeIP6Addr
		}
		out = append(out,
			nftMap{set: &nftables.Set{Table: t, Name: steerMap(f), IsMap: true, KeyType: typ, DataType: typ}, elems: fwd},
			nftMap{set: &nftables.Set{Table: t, Name: restoreMap(f), IsMap: true, KeyType: typ, DataType: typ}, elems: back},
		)
	}
	return out
}

type nftMap struct {
	set   *nftables.Set
	elems []nftables.SetElement
}

// nftExprs builds the netlink expressions that actually get applied for a Rule.
// It is the applied sibling of nftText; both are driven from the same Rule so
// they cannot describe different rules. sets resolves a map's name to the set
// added in the same transaction, whose ID a lookup carries.
func nftExprs(r Rule, sets map[string]*nftables.Set) []expr.Any {
	var e []expr.Any

	// An inet table hands every rule packets of both families, and a payload
	// load reads fixed offsets whatever the packet is. A rule with a family
	// matches it first, so an address load never reads an IPv6 header at IPv4
	// offsets. nft adds the same match when it parses `ip saddr` in an inet
	// table.
	fam, hasFam := r.family()
	if hasFam {
		e = append(e,
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{nfproto(fam)}},
		)
	}

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
		e = append(e, ipCmp(*r.SrcAddr, true)...)
	}
	if r.DstAddr != nil {
		e = append(e, ipCmp(*r.DstAddr, false)...)
	}

	// Match the L4 protocol, then the port (source or destination). A rule with
	// no protocol matches every flow arriving on its interface, which is what
	// the conntrack save rule wants: the link carries one mapping's traffic and
	// nothing else, so the peer it names is the whole match.
	if r.Port.Proto != "" {
		e = append(e,
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoNum(r.Port.Proto)}},
		)
		e = append(e, portMatch(r.Port, r.PortIsSrc)...)
	}

	// The rewrite comes before the counter, so a lookup that misses ends the
	// rule uncounted and the counter shows packets actually rewritten.
	switch r.Kind {
	case KindSteer:
		e = append(e, rewriteExprs(fam, false, lookupSet(sets, steerMap(fam)))...)
		return append(e, &expr.Counter{})
	case KindRestore:
		e = append(e, rewriteExprs(fam, true, lookupSet(sets, restoreMap(fam)))...)
		return append(e, &expr.Counter{})
	}

	e = append(e, &expr.Counter{})

	switch r.Kind {
	case KindDNAT:
		e = append(e, dnatExprs(r.ToAddr, r.Port, r.ToPort)...)
	case KindSNAT:
		e = append(e, snatIdentityExprs(fam)...)
	case KindMark:
		e = append(e, markExprs(r.Mark)...)
	case KindCtSave:
		e = append(e, ctSaveExprs(r.Mark)...)
	case KindCtLoad:
		e = append(e, ctLoadExprs()...)
	}
	return e
}

// Byte offsets into the IPv4 and IPv6 headers for the source and destination
// address.
const (
	ipSrcOffset  uint32 = 12
	ipDstOffset  uint32 = 16
	ip6SrcOffset uint32 = 8
	ip6DstOffset uint32 = 24
)

// addrLoad is where a family keeps the source or destination address in its
// network header, and how long the address is.
func addrLoad(f Family, src bool) (offset, length uint32) {
	switch {
	case f == FamilyIPv6 && src:
		return ip6SrcOffset, 16
	case f == FamilyIPv6:
		return ip6DstOffset, 16
	case src:
		return ipSrcOffset, 4
	default:
		return ipDstOffset, 4
	}
}

// nfproto is the netfilter protocol number a family's packets carry, which is
// what both the meta nfproto match and the NAT statement's family name.
func nfproto(f Family) byte {
	if f == FamilyIPv6 {
		return unix.NFPROTO_IPV6
	}
	return unix.NFPROTO_IPV4
}

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

// ipCmp loads the source or destination address from the network header, at
// the offsets of the address's own family, and compares it for equality.
func ipCmp(a netip.Addr, src bool) []expr.Any {
	offset, length := addrLoad(familyOf(a), src)
	return []expr.Any{
		&expr.Payload{
			OperationType: expr.PayloadLoad,
			DestRegister:  1,
			Base:          expr.PayloadBaseNetworkHeader,
			Offset:        offset,
			Len:           length,
		},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: addrBytes(a)},
	}
}

// addrBytes is an address in network order: 4 bytes for IPv4, 16 for IPv6.
// A register holds 16 bytes, so either fits register 1.
func addrBytes(a netip.Addr) []byte {
	if familyOf(a) == FamilyIPv4 {
		v4 := a.Unmap().As4()
		return v4[:]
	}
	v6 := a.As16()
	return v6[:]
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

// dnatExprs rewrites the destination to a pod address and port range. A
// non-zero toPort replaces the single matched port with that port. The NAT
// carries the target's family, which an inet table needs to know how wide the
// address in register 1 is.
func dnatExprs(to netip.Addr, p PortSel, toPort uint16) []expr.Any {
	if toPort != 0 {
		p = PortSel{Proto: p.Proto, First: toPort, Last: toPort}
	}
	out := []expr.Any{
		&expr.Immediate{Register: 1, Data: addrBytes(to)},
		&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(p.First)},
	}
	nat := &expr.NAT{
		Type:        expr.NATTypeDestNAT,
		Family:      uint32(nfproto(familyOf(to))),
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

// snatIdentityExprs is the identity source NAT (snat ip to ip saddr, or the
// ip6 form): load the packet's own source address and SNAT to it, claiming the
// connection before the CNI's masquerade can rewrite it.
func snatIdentityExprs(f Family) []expr.Any {
	offset, length := addrLoad(f, true)
	return []expr.Any{
		&expr.Payload{
			OperationType: expr.PayloadLoad,
			DestRegister:  1,
			Base:          expr.PayloadBaseNetworkHeader,
			Offset:        offset,
			Len:           length,
		},
		&expr.NAT{
			Type:       expr.NATTypeSourceNAT,
			Family:     uint32(nfproto(f)),
			RegAddrMin: 1,
			RegAddrMax: 1,
		},
	}
}

// lookupSet returns the named set from the transaction, or a set carrying the
// name alone, which a lookup resolves against the kernel's table.
func lookupSet(sets map[string]*nftables.Set, name string) *nftables.Set {
	if s, ok := sets[name]; ok {
		return s
	}
	return &nftables.Set{Name: name}
}

// rewriteExprs replaces the destination (or, with src, the source) address
// with what the map holds for it. A lookup that misses breaks the rule and
// leaves the packet as it was.
//
// IPv4 needs its header checksum fixed, and the TCP or UDP checksum through
// the pseudo-header flag; the kernel skips a UDP checksum of zero. IPv6 has no
// header checksum, and steerV6 picks a stand-in with the same ones' complement
// sum as the address it replaces, so the transport checksum stays correct with
// no update. That also keeps clear of google/nftables, which sends the
// pseudo-header flag only together with a checksum type.
func rewriteExprs(f Family, src bool, set *nftables.Set) []expr.Any {
	offset, length := addrLoad(f, src)
	write := &expr.Payload{
		OperationType:  expr.PayloadWrite,
		SourceRegister: 1,
		Base:           expr.PayloadBaseNetworkHeader,
		Offset:         offset,
		Len:            length,
	}
	if f == FamilyIPv4 {
		write.CsumType = expr.CsumTypeInet
		write.CsumOffset = ipv4CsumOffset
		write.CsumFlags = unix.NFT_PAYLOAD_L4CSUM_PSEUDOHDR
	}
	return []expr.Any{
		&expr.Payload{
			OperationType: expr.PayloadLoad,
			DestRegister:  1,
			Base:          expr.PayloadBaseNetworkHeader,
			Offset:        offset,
			Len:           length,
		},
		&expr.Lookup{
			SourceRegister: 1,
			DestRegister:   1,
			IsDestRegSet:   true,
			SetName:        set.Name,
			SetID:          set.ID,
		},
		write,
	}
}

// ipv4CsumOffset is where the IPv4 header keeps its checksum.
const ipv4CsumOffset = 10

// markExprs sets the fwmark that steers a reply back to the accepting node.
func markExprs(mark uint32) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	}
}

// ctSaveExprs writes a peer's mark straight into the flow's conntrack entry.
// It sets ct mark without touching meta mark, so the request keeps travelling
// to the pod rather than matching the divert rule on its way in.
func ctSaveExprs(mark uint32) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)},
		&expr.Ct{Key: expr.CtKeyMARK, SourceRegister: true, Register: 1},
	}
}

// ctLoadExprs copies the flow's stored ct mark onto the packet, which is what
// the divert rule reads. A flow with no entry yields 0 and leaves by this
// node's own uplink.
func ctLoadExprs() []expr.Any {
	return []expr.Any{
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	}
}

// applyNFT writes the whole kuport table in one transaction: remove a leftover
// ip table, delete and re-add the inet table, add the maps, chains and rules,
// then flush the connection. Rewriting the entire table each pass is what makes
// a deleted mapping disappear by absence. The table is deleted rather than
// flushed because the kernel refuses to change a live base chain's hook,
// priority or type, so an upgrade that reshapes a chain needs it.
func applyNFT(conn NFTConn, plan Plan) error {
	delTable(conn, legacyTable())

	t := nftTable()
	delTable(conn, t)
	conn.AddTable(t)

	sets := map[string]*nftables.Set{}
	for _, m := range nftMaps(t, plan.Steer) {
		if err := conn.AddSet(m.set, m.elems); err != nil {
			return fmt.Errorf("add map %s: %w", m.set.Name, err)
		}
		sets[m.set.Name] = m.set
	}

	chainObjs := map[string]*nftables.Chain{}
	for _, c := range nftChains(t) {
		conn.AddChain(c)
		chainObjs[c.Name] = c
	}
	for _, r := range plan.Rules {
		conn.AddRule(&nftables.Rule{
			Table: t,
			Chain: chainObjs[r.Chain],
			Exprs: nftExprs(r, sets),
		})
	}
	return conn.Flush()
}

// teardownNFT removes the whole kuport table, and the ip table an older
// release left behind.
func teardownNFT(conn NFTConn) error {
	delTable(conn, legacyTable())
	delTable(conn, nftTable())
	return conn.Flush()
}
