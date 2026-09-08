package datapath

import "net/netip"

// State is the complete desired host state for ONE node. The reconcile computes
// it; Apply makes the host match it. A mapping that was deleted disappears by
// being absent from a later State, never by anything remembering to remove it.
type State struct {
	DNAT   []DNATRule
	Exempt []ExemptRule
	Mark   []MarkRule
	CtSave []CtSaveRule
	CtLoad []CtLoadRule
	Links  []Link
	Rules  []IPRule
	Routes []Route
}

// PortSel is a protocol and an inclusive port range. Last == First for a single
// port, which is the common case.
type PortSel struct {
	Proto string // "tcp" or "udp"
	First uint16
	Last  uint16
}

// DNATRule runs on an accepting node: one per class interface per mapping.
type DNATRule struct {
	Iface  string
	Port   PortSel
	ToAddr netip.Addr
}

// ExemptRule is an identity SNAT that claims the connection before the CNI's
// masquerade can. Negate renders `oifname != <OifName>`.
type ExemptRule struct {
	OifName   string
	Negate    bool
	DstAddr   *netip.Addr
	SrcAddr   *netip.Addr
	Port      PortSel
	PortIsSrc bool
}

// MarkRule runs on a target node, marking replies so they route back through
// the accepting node rather than out this node's own uplink.
type MarkRule struct {
	SrcAddr netip.Addr
	Port    PortSel // matched as source port
	Mark    uint32
}

// CtSaveRule runs on a target node under Multi serving. A request arriving on
// one accepting node's link writes that peer's mark into the flow's conntrack
// entry, which is the only record of which node forwarded it. It sets ct mark
// directly and leaves the packet's own mark alone: a marked request would match
// the divert rule and be routed straight back out the link it came in on.
type CtSaveRule struct {
	Iface string
	Mark  uint32
}

// CtLoadRule runs on a target node under Multi serving, restoring the mark
// CtSaveRule stored so the reply routes back over the link its request arrived
// on. It replaces the static MarkRule, which cannot tell peers apart. A flow
// with no conntrack entry restores 0, matches no divert rule, and leaves by
// this node's own uplink; the next inbound packet writes the mark again.
type CtLoadRule struct {
	SrcAddr netip.Addr
	Port    PortSel // matched as source port
}

// Link is one point-to-point VXLAN to a peer node.
type Link struct {
	Name       string // kup-<first 8 hex of sha256(peer node name)>
	VNI        uint32
	Port       uint16
	LocalAddr  netip.Addr
	RemoteAddr netip.Addr
	LinkAddr   netip.Prefix // this node's end, a /31
	MTU        uint32       // 0 leaves the device at the kernel default
}

type IPRule struct {
	Pref  uint32
	Mark  uint32
	To    *netip.Prefix // set only on the loop-guard rule
	Table uint32
}

type Route struct {
	Table uint32
	Via   netip.Addr
	Dev   string
}
