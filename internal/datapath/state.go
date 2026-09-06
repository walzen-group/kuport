package datapath

import "net/netip"

// State is the complete desired host state for ONE node. The reconcile computes
// it; Apply makes the host match it. A mapping that was deleted disappears by
// being absent from a later State, never by anything remembering to remove it.
type State struct {
	DNAT   []DNATRule
	Exempt []ExemptRule
	Mark   []MarkRule
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

// Link is one point-to-point VXLAN to a peer node.
type Link struct {
	Name       string // kup-<first 8 hex of sha256(peer node name)>
	VNI        uint32
	Port       uint16
	LocalAddr  netip.Addr
	RemoteAddr netip.Addr
	LinkAddr   netip.Prefix // this node's end, a /31
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
