package datapath

import (
	"encoding/binary"
	"net/netip"
	"sort"
)

// A host firewall such as Talos's drops a new connection to one of the node's
// own addresses from a filter chain in prerouting at -110. kup-pre cannot
// translate ahead of it: the kernel runs every nat chain from one hook at the
// dstnat priority, -100, whatever priority the chain itself declares. So
// kup-steer, a filter chain at -120, rewrites a mapped packet's destination
// from the node's own address to a stand-in address the node does not hold.
//
// By then conntrack (-200) has recorded the address the client dialed, so it
// writes that address back as the reply's source once kup-pre's DNAT is
// undone, and a mesh's rules at mangle (-150), netbird's mark for a local
// destination among them, have seen the real address too. The firewall sees a
// destination that is not the node's and accepts the packet, and kup-pre
// translates the stand-in to the target as before. docs/datapath.md has the
// walk.

// SteerV4 and SteerV6 are the blocks the stand-in addresses come from. No
// node may hold an address inside them, and a class's link subnets must stay
// out of SteerV4.
var (
	SteerV4 = netip.MustParsePrefix("169.254.76.0/24")
	SteerV6 = netip.MustParsePrefix("fd6b:7570::/96")
)

// Steer pairs one of the node's own addresses with its stand-in.
type Steer struct {
	Local   netip.Addr
	Virtual netip.Addr
}

// steerPairs gives every local address a stand-in in its own family, in sorted
// address order. The link ends are local addresses as well: a request arriving
// over a return link is addressed to this node's end of it. A stand-in that
// moves to another address affects no flow: conntrack holds the dialed address
// and the DNAT target, and the stand-in only lives between kup-steer and
// kup-pre.
func steerPairs(s State) []Steer {
	seen := map[netip.Addr]bool{}
	var locals []netip.Addr
	add := func(a netip.Addr) {
		a = a.Unmap()
		if !a.IsValid() || seen[a] || SteerV4.Contains(a) || SteerV6.Contains(a) {
			return
		}
		seen[a] = true
		locals = append(locals, a)
	}
	for _, a := range s.LocalAddrs {
		add(a)
	}
	for _, l := range s.Links {
		if l.LinkAddr.IsValid() {
			add(l.LinkAddr.Addr())
		}
		if l.LinkAddr6.IsValid() {
			add(l.LinkAddr6.Addr())
		}
	}
	sort.Slice(locals, func(i, j int) bool { return locals[i].Less(locals[j]) })

	var out []Steer
	n4, n6 := 0, 0
	for _, a := range locals {
		if a.Is4() {
			// .0 and .255 stay unused, which leaves 254 stand-ins.
			if n4 == 254 {
				continue
			}
			n4++
			out = append(out, Steer{Local: a, Virtual: steerV4(n4)})
			continue
		}
		n6++
		out = append(out, Steer{Local: a, Virtual: steerV6(n6, a)})
	}
	return out
}

func steerV4(slot int) netip.Addr {
	b := SteerV4.Addr().As4()
	b[3] = byte(slot)
	return netip.AddrFrom4(b)
}

// steerV6 builds the IPv6 stand-in for local in the given slot. Its last word
// is chosen so the stand-in's ones' complement sum equals local's, the way
// RFC 6296 keeps NPTv6 checksum-neutral: the TCP and UDP checksums cover the
// addresses through the pseudo-header, so a checksum-neutral rewrite leaves
// them correct with no update, whether the packet carries a full checksum or,
// from a pod's veth, a partial one.
func steerV6(slot int, local netip.Addr) netip.Addr {
	b := SteerV6.Addr().As16()
	binary.BigEndian.PutUint16(b[12:], uint16(slot))
	binary.BigEndian.PutUint16(b[14:], 0)
	want := onesSum(local.As16())
	have := onesSum(b)
	binary.BigEndian.PutUint16(b[14:], onesAdd(want, ^have))
	return netip.AddrFrom16(b)
}

// onesSum is the 16-bit ones' complement sum of an address's words.
func onesSum(b [16]byte) uint16 {
	var s uint16
	for i := 0; i < 16; i += 2 {
		s = onesAdd(s, binary.BigEndian.Uint16(b[i:]))
	}
	return s
}

func onesAdd(a, b uint16) uint16 {
	s := uint32(a) + uint32(b)
	return uint16(s&0xffff + s>>16)
}
