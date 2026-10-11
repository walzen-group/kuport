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

	// Egress is what this node does with connections that pods an Egress
	// selects open toward addresses outside ClusterCIDRs.
	Egress []EgressRule
	// EgressGateway is the egress node's half: masquerade what arrives on a
	// link and route the replies back into it.
	EgressGateway []EgressGatewayRule
	// ClusterCIDRs are the destinations an egress rule leaves alone: every
	// node's pod CIDRs and InternalIPs, in both families.
	ClusterCIDRs []netip.Prefix

	// LocalAddrs are the node's own addresses, the destinations a host
	// firewall protects. A mapped packet addressed to one of them is steered
	// past that firewall; steer.go says how.
	LocalAddrs []netip.Addr
}

// PortSel is a protocol and an inclusive port range. Last == First for a single
// port, which is the common case.
type PortSel struct {
	Proto string // "tcp" or "udp"
	First uint16
	Last  uint16
}

// DNATRule rewrites a destination. On an accepting node there is one per class
// interface per mapping, translating to whatever carries the request onward.
// On a target node under Multi serving there is one per return link,
// translating a request that arrived over that link to the local pod, and
// DstAddr is what keeps it from catching anything else on the device.
type DNATRule struct {
	Iface   string
	Port    PortSel
	ToAddr  netip.Addr
	ToPort  uint16      // translated destination port; 0 keeps the matched port
	DstAddr *netip.Addr // ip daddr match; nil means none
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

// EgressRule runs on the node holding a pod an Egress selects, and marks only
// connections the pod opened (ct direction original) toward a destination
// outside ClusterCIDRs. The routing rules the mark selects sit after every
// other policy rule and consult main with suppress_prefixlength 0 first, so a
// destination with a more specific route keeps it, and only traffic bound for
// the default route goes into the link to the egress node, or, under
// EgressBlockedMark, into a blackhole.
type EgressRule struct {
	SrcAddr netip.Addr
	Mark    uint32
}

// EgressGatewayRule runs on the egress node for one link from a pod node. It
// masquerades the connections arriving on Iface to the address of the uplink
// they leave by, writes Mark into their conntrack entries, and restores Mark
// onto their replies, so the pref 102 rule routes each reply back into Iface.
type EgressGatewayRule struct {
	Iface string
	Mark  uint32
}

// Link is one point-to-point VXLAN to a peer node. The underlay is IPv4; the
// device carries IPv6 inside it when LinkAddr6 is set.
type Link struct {
	Name       string // kup-<first 8 hex of sha256(peer node name)>
	VNI        uint32
	Port       uint16
	LocalAddr  netip.Addr
	RemoteAddr netip.Addr
	LinkAddr   netip.Prefix // this node's end, a /31
	LinkAddr6  netip.Prefix // this node's end, a /127; the zero Prefix means no IPv6
	MTU        uint32       // 0 leaves the device at the kernel default
}

// Family is the address family of a routing rule. The zero value is IPv4, so a
// rule written before IPv6 existed keeps its meaning.
type Family uint8

const (
	FamilyIPv4 Family = iota
	FamilyIPv6
)

type IPRule struct {
	Pref   uint32
	Mark   uint32
	To     *netip.Prefix // set only on the loop-guard rule
	Table  uint32
	Family Family
	// SuppressDefault renders suppress_prefixlength 0: the lookup ignores a
	// default route in Table and falls through to the next rule, so only a
	// more specific route answers.
	SuppressDefault bool
}

// Route is a table's default route: via a link's far end, or a blackhole that
// drops whatever reaches it. A route via a gateway takes the gateway's family;
// a blackhole names its family.
type Route struct {
	Table uint32
	Via   netip.Addr
	Dev   string

	Blackhole       bool
	BlackholeFamily Family
}

func (r Route) family() Family {
	if r.Blackhole {
		return r.BlackholeFamily
	}
	return familyOf(r.Via)
}

// EgressBlockedTable holds the blackhole default routes a blocked Egress pod's
// traffic reaches, and EgressBlockedMark selects it. Both sit outside every
// link slot's numbers, and kuport owns the table: Apply reconciles it on
// every pass and Teardown empties it.
const (
	EgressBlockedTable uint32 = 11024
	EgressBlockedMark  uint32 = 0x6b70f000
)
