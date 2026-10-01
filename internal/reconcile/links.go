package reconcile

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"net/netip"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// defaultSubnet is the class return-path subnet when none is set: 128 /31 slots.
const defaultSubnet = "169.254.77.0/24"

// defaultSubnet6 is the IPv6 return-path subnet when none is set: 32768 /127
// slots, of which a slot uses the one its /31 does. The 40 bits after fd are
// random, per RFC 4193.
const defaultSubnet6 = "fd64:f5ac:e961::/112"

// ipv6MinMTU is the smallest MTU a device may have and still carry IPv6. The
// kernel refuses an IPv6 address on a smaller device, and that refusal would
// fail the whole apply, IPv4 included.
const ipv6MinMTU = 1280

// markBase is OR'd with a slot to form the packet mark that keeps a return
// diversion narrow. The high bits spell "kp".
const markBase uint32 = 0x6b700000

// neededPair is a node pair that needs a return link, with the set of nodes that
// hold a chosen endpoint on it. Only a holder proposes a new claim.
type neededPair struct {
	key     string
	peers   [2]string
	holders map[string]bool
}

// classAlloc is the settled slot allocation for one class in this pass: the
// existing claims read from status, the slots newly assigned to needed pairs
// that had none, and whether the subnet ran out or failed to parse. A newly
// assigned slot exists only to propose its claim; nothing that carries the
// slot into the host is built until the claim lands in existing.
type classAlloc struct {
	subnet        netip.Prefix
	invalidSubnet bool
	// subnet6 is the IPv6 subnet the links' /127s come from. It is invalid,
	// and the links carry IPv4 alone, when the class's subnet6 does not parse
	// as IPv6 or holds fewer /127s than subnet holds /31s.
	subnet6        netip.Prefix
	invalidSubnet6 bool
	exhausted      bool
	existing       map[string]v1alpha1.LinkAllocation
	needed         map[string]neededPair
	newly          map[string]int // key -> slot, to propose as a claim
}

// neededPairsByClass groups the return-link pairs every accepted mapping needs,
// keyed by class name. A pair is needed for each programming node that sits off
// the chosen endpoint's node. Under Single that is at most one pair; under
// Multi it is one per accepting node that lacks the pod, since each of them
// forwards and each needs its own way back.
func neededPairsByClass(mappings []*mapping) map[string]map[string]neededPair {
	out := map[string]map[string]neededPair{}
	for _, m := range mappings {
		if !m.accepted || m.endpoint == nil || m.effAccepting == "" || !m.remote {
			continue
		}
		cls := m.pm.Spec.ClassName
		for _, peer := range remoteProgrammers(m, m.endpoint.node) {
			key := pairKey(peer, m.endpoint.node)
			byKey := out[cls]
			if byKey == nil {
				byKey = map[string]neededPair{}
				out[cls] = byKey
			}
			p, ok := byKey[key]
			if !ok {
				p = neededPair{key: key, peers: sortedPair(peer, m.endpoint.node), holders: map[string]bool{}}
			}
			p.holders[m.endpoint.node] = true
			byKey[key] = p
		}
	}
	return out
}

// allocateClass settles the slot for every needed pair of a class. Existing
// claims keep their slot untouched; a needed pair without a claim takes the
// lowest free slot, assigned in sorted key order so every agent computes the
// same map.
func allocateClass(class *v1alpha1.PortMapClass, needed map[string]neededPair) classAlloc {
	a := classAlloc{
		existing: map[string]v1alpha1.LinkAllocation{},
		needed:   needed,
		newly:    map[string]int{},
	}

	subnet, err := parseClassSubnet(class)
	if err != nil {
		a.invalidSubnet = true
		return a
	}
	a.subnet = subnet
	total := slotCount(subnet)

	if s6, err := parseClassSubnet6(class, total); err == nil {
		a.subnet6 = s6
	} else {
		a.invalidSubnet6 = true
	}

	used := map[int]bool{}
	for _, la := range class.Status.Links {
		a.existing[la.Key] = la
		used[int(la.Slot)] = true
	}

	keys := make([]string, 0, len(needed))
	for k := range needed {
		if _, ok := a.existing[k]; ok {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		slot := lowestFree(used, total)
		if slot < 0 {
			a.exhausted = true
			continue
		}
		a.newly[k] = slot
		used[slot] = true
	}
	return a
}

// landedSlotFor returns the slot of the return-link pair between one peer and
// the mapping's endpoint node, when the claim has landed in the class status,
// and false while it is still propagating. Marks, divert rules, links and
// routes all carry the slot; emitting any of them against a tentative
// allocation lets a lost claim race mark packets with a slot another pair
// already holds. It takes the peer rather than reading one off the mapping
// because Multi serving gives the pod's node a link per remote programmer,
// each on its own slot, so a reply can leave by the link its request arrived on.
func (a classAlloc) landedSlotFor(m *mapping, peer string) (int, bool) {
	key := pairKey(peer, m.endpoint.node)
	la, ok := a.existing[key]
	return int(la.Slot), ok
}

// linkParams holds every value derived from a slot for one end of a return link.
// All of it is fixed by the slot and the two node names, so both ends and every
// agent compute it identically.
type linkParams struct {
	slot       int
	name       string
	localAddr  netip.Addr // this node's outer (node) address
	remoteAddr netip.Addr // peer's outer (node) address
	thisEnd    netip.Addr // this node's /31 address
	peerEnd    netip.Addr // peer's /31 address
	linkAddr   netip.Prefix
	// The IPv6 ends, set when the class's subnet6 is usable and the link is
	// large enough for IPv6; has6 says which.
	has6       bool
	thisEnd6   netip.Addr
	peerEnd6   netip.Addr
	linkAddr6  netip.Prefix
	peerNode32 netip.Prefix // peer node address as a /32, for the loop guard
	mark       uint32
	table      uint32
	vni        uint32
	port       uint16
	mtu        uint32 // 0 when neither node has reported an underlay yet
}

// buildLinkParams derives the link for the pair (thisNode, peer) at a slot. It
// returns false when either node's address or the subnet cannot be resolved.
func buildLinkParams(idx *index, class *v1alpha1.PortMapClass, a classAlloc, thisNode, peer string, slot int) (linkParams, bool) {
	local, err1 := netip.ParseAddr(nodeAddrOf(idx, thisNode))
	remote, err2 := netip.ParseAddr(nodeAddrOf(idx, peer))
	if err1 != nil || err2 != nil {
		return linkParams{}, false
	}

	// The lower-named node takes the low address of each pair, in both
	// families, so both ends compute each other's addresses from the slot.
	first := thisNode == sortedPair(thisNode, peer)[0]
	ends := func(lo, hi netip.Addr) (netip.Addr, netip.Addr) {
		if first {
			return lo, hi
		}
		return hi, lo
	}
	thisEnd, peerEnd := ends(nthSlash31(a.subnet, slot))

	// The kernel keys a VXLAN device by VNI and UDP port, so two links on one
	// node cannot share both. Under Multi serving the pod's node holds a link
	// per remote accepting node, so the VNI carries the slot: the class's VNI
	// is the base and each pair sits at base+slot. Both ends derive it from the
	// same recorded slot, so they agree without negotiating.
	vni, port := vxlanParams(class)
	lp := linkParams{
		slot:       slot,
		name:       linkName(class.Name, peer),
		localAddr:  local,
		remoteAddr: remote,
		thisEnd:    thisEnd,
		peerEnd:    peerEnd,
		linkAddr:   netip.PrefixFrom(thisEnd, 31),
		peerNode32: netip.PrefixFrom(remote, 32),
		mark:       markBase | uint32(slot),
		table:      200 + uint32(slot),
		vni:        vni + uint32(slot),
		port:       port,
		mtu:        linkMTUFor(class, thisNode, peer, local.Is4() && remote.Is4()),
	}

	// Both ends read the same subnet6 and the same two MTU rows, so they agree
	// on whether the link carries IPv6 as they agree on everything else.
	if a.subnet6.IsValid() && (lp.mtu == 0 || lp.mtu >= ipv6MinMTU) {
		lp.has6 = true
		lp.thisEnd6, lp.peerEnd6 = ends(nthSlash127(a.subnet6, slot))
		lp.linkAddr6 = netip.PrefixFrom(lp.thisEnd6, 127)
	}
	return lp, true
}

// linkMTUFor sizes the device to the path it rides: the smaller of the two
// nodes' underlays less the encapsulation. Each agent reads both figures from
// the class status, so the two ends agree without exchanging anything. A peer
// that has not reported yet leaves the link at this node's own figure, and the
// pass that follows the peer's first status write shrinks it.
func linkMTUFor(class *v1alpha1.PortMapClass, thisNode, peer string, outerIsV4 bool) uint32 {
	mtu := datapath.LinkMTUBetween(
		underlayMTUOf(class, thisNode),
		underlayMTUOf(class, peer),
		outerIsV4,
	)
	if mtu <= 0 {
		return 0
	}
	return uint32(mtu)
}

// underlayMTUOf reads one node's reported underlay from the class status, or 0
// when that node has written no row.
func underlayMTUOf(class *v1alpha1.PortMapClass, name string) int {
	if row := classNodeRow(class, name); row != nil {
		return int(row.UnderlayMTU)
	}
	return 0
}

// vxlanParams returns the VNI and UDP port for a class's return links, with the
// documented defaults when the vxlan block is absent.
func vxlanParams(class *v1alpha1.PortMapClass) (vni uint32, port uint16) {
	vni, port = 4242, 4790
	if v := class.Spec.ReturnPath.Vxlan; v != nil {
		if v.VNI != 0 {
			vni = uint32(v.VNI)
		}
		if v.Port != 0 {
			port = uint16(v.Port)
		}
	}
	return
}

// parseClassSubnet parses a class's return-path subnet, masked to its prefix,
// falling back to the default when unset.
func parseClassSubnet(class *v1alpha1.PortMapClass) (netip.Prefix, error) {
	s := defaultSubnet
	if v := class.Spec.ReturnPath.Vxlan; v != nil && v.Subnet != "" {
		s = v.Subnet
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("subnet %q is not IPv4", s)
	}
	return p.Masked(), nil
}

// parseClassSubnet6 parses a class's IPv6 return-path subnet, masked to its
// prefix, falling back to the default when unset. A slot indexes both subnets,
// so subnet6 must hold at least slots /127s.
func parseClassSubnet6(class *v1alpha1.PortMapClass, slots int) (netip.Prefix, error) {
	s := defaultSubnet6
	if v := class.Spec.ReturnPath.Vxlan; v != nil && v.Subnet6 != "" {
		s = v.Subnet6
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !p.Addr().Is6() || p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("subnet6 %q is not IPv6", s)
	}
	// A /(128-host) holds 2^(host-1) /127s, and a /128 holds none.
	if host := 128 - p.Bits(); host < 1 || (host-1 < 31 && 1<<(host-1) < slots) {
		return netip.Prefix{}, fmt.Errorf("subnet6 %q holds fewer than %d /127s", s, slots)
	}
	return p.Masked(), nil
}

// nthSlash127 returns the two addresses of the slot-th /127 within an IPv6
// subnet, the IPv6 twin of nthSlash31.
func nthSlash127(subnet netip.Prefix, slot int) (netip.Addr, netip.Addr) {
	b := subnet.Addr().As16()
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	at := func(off uint64) netip.Addr {
		l, carry := bits.Add64(lo, off, 0)
		var a [16]byte
		binary.BigEndian.PutUint64(a[:8], hi+carry)
		binary.BigEndian.PutUint64(a[8:], l)
		return netip.AddrFrom16(a)
	}
	off := uint64(slot) * 2
	return at(off), at(off + 1)
}

// slotCount is how many /31s a subnet holds.
func slotCount(subnet netip.Prefix) int {
	host := 32 - subnet.Bits()
	if host <= 1 {
		return 1 << 0
	}
	return 1 << (host - 1)
}

// nthSlash31 returns the two addresses of the slot-th /31 within a subnet.
func nthSlash31(subnet netip.Prefix, slot int) (netip.Addr, netip.Addr) {
	base := subnet.Addr().As4()
	v := binary.BigEndian.Uint32(base[:]) + uint32(slot)*2
	var a, b [4]byte
	binary.BigEndian.PutUint32(a[:], v)
	binary.BigEndian.PutUint32(b[:], v+1)
	return netip.AddrFrom4(a), netip.AddrFrom4(b)
}

// lowestFree returns the lowest slot in [0,total) not in used, or -1 when none.
func lowestFree(used map[int]bool, total int) int {
	for s := 0; s < total; s++ {
		if !used[s] {
			return s
		}
	}
	return -1
}

// linkName is the vxlan device name for one class's link to a peer: a short
// hash of the class name and the peer's node name, so both ends and every agent
// name the same device.
//
// The class is in the hash because two classes can want a link between the same
// pair of nodes, each on its own subnet and VNI. Hashing the peer alone gave
// them one device name, so whichever class applied second added a route via an
// address the existing device did not carry and failed with "network is
// unreachable", which parked that node's agent and with it the DaemonSet.
func linkName(class, peer string) string {
	sum := sha256.Sum256([]byte(class + "/" + peer))
	return "kup-" + hex.EncodeToString(sum[:])[:8]
}

// pairKey is the two node names sorted and joined with a slash, the LinkAllocation key.
func pairKey(a, b string) string {
	p := sortedPair(a, b)
	return p[0] + "/" + p[1]
}

// sortedPair returns the two node names in ascending order.
func sortedPair(a, b string) [2]string {
	if a <= b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

// gcClaims walks a class's existing claims and returns the claim updates and
// drops this pass calls for. A needed pair clears UnusedSince; an unneeded pair
// sets it once to now; a pair unused for more than 24h is dropped. Any selecting
// agent may run this; the write conflicts sort themselves out.
func gcClaims(class *v1alpha1.PortMapClass, needed map[string]neededPair, now metav1.Time) (updates []v1alpha1.LinkAllocation, drops []string) {
	subnet, subnetErr := parseClassSubnet(class)

	for i := range class.Status.Links {
		la := class.Status.Links[i]
		_, isNeeded := needed[la.Key]

		if !isNeeded && la.UnusedSince != nil && now.Sub(la.UnusedSince.Time) > 24*time.Hour {
			drops = append(drops, la.Key)
			continue
		}

		cp := la
		changed := false

		switch {
		case isNeeded && la.UnusedSince != nil:
			cp.UnusedSince = nil
			changed = true
		case !isNeeded && la.UnusedSince == nil:
			t := now
			cp.UnusedSince = &t
			changed = true
		}

		// A link's addresses are derived from the class subnet and the slot
		// every pass, never read back from here, so changing the subnet leaves
		// this field naming a /31 nothing carries. A claim keeps its slot, so
		// correcting the text is the whole repair.
		if subnetErr == nil && int(la.Slot) >= 0 && int(la.Slot) < slotCount(subnet) {
			lo, _ := nthSlash31(subnet, int(la.Slot))
			if want := netip.PrefixFrom(lo, 31).String(); want != cp.Subnet {
				cp.Subnet = want
				changed = true
			}
		}

		if changed {
			updates = append(updates, cp)
		}
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Key < updates[j].Key })
	sort.Strings(drops)
	return
}
