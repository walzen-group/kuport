package reconcile

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// defaultSubnet is the class return-path subnet when none is set: 128 /31 slots.
const defaultSubnet = "169.254.77.0/24"

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
	exhausted     bool
	existing      map[string]v1alpha1.LinkAllocation
	needed        map[string]neededPair
	newly         map[string]int // key -> slot, to propose as a claim
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

// landedSlot returns the slot of a mapping's return-link pair when the claim
// has landed in the class status, and false while it is still propagating.
// Marks, divert rules, links and routes all carry the slot; emitting any of
// them against a tentative allocation lets a lost claim race mark packets
// with a slot another pair already holds.
func (a classAlloc) landedSlot(m *mapping) (int, bool) {
	return a.landedSlotFor(m, m.effAccepting)
}

// landedSlotFor is landedSlot for one named peer, which is what Multi serving
// needs: the pod's node holds a link per remote programmer, each on its own
// slot, so a reply can leave by the link its request arrived on.
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
	peerNode32 netip.Prefix // peer node address as a /32, for the loop guard
	mark       uint32
	table      uint32
	vni        uint32
	port       uint16
}

// buildLinkParams derives the link for the pair (thisNode, peer) at a slot. It
// returns false when either node's address or the subnet cannot be resolved.
func buildLinkParams(idx *index, class *v1alpha1.PortMapClass, subnet netip.Prefix, thisNode, peer string, slot int) (linkParams, bool) {
	local, err1 := netip.ParseAddr(nodeAddrOf(idx, thisNode))
	remote, err2 := netip.ParseAddr(nodeAddrOf(idx, peer))
	if err1 != nil || err2 != nil {
		return linkParams{}, false
	}

	lo, hi := nthSlash31(subnet, slot)
	pair := sortedPair(thisNode, peer)
	var thisEnd, peerEnd netip.Addr
	if thisNode == pair[0] {
		thisEnd, peerEnd = lo, hi
	} else {
		thisEnd, peerEnd = hi, lo
	}

	vni, port := vxlanParams(class)
	return linkParams{
		slot:       slot,
		name:       linkName(peer),
		localAddr:  local,
		remoteAddr: remote,
		thisEnd:    thisEnd,
		peerEnd:    peerEnd,
		linkAddr:   netip.PrefixFrom(thisEnd, 31),
		peerNode32: netip.PrefixFrom(remote, 32),
		mark:       markBase | uint32(slot),
		table:      200 + uint32(slot),
		vni:        vni,
		port:       port,
	}, true
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

// linkName is the vxlan device name for the link to a peer: a short hash of the
// peer's node name, so both ends and every agent name the same device.
func linkName(peer string) string {
	sum := sha256.Sum256([]byte(peer))
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
	for i := range class.Status.Links {
		la := class.Status.Links[i]
		_, isNeeded := needed[la.Key]
		switch {
		case isNeeded:
			if la.UnusedSince != nil {
				cp := la
				cp.UnusedSince = nil
				updates = append(updates, cp)
			}
		case la.UnusedSince == nil:
			cp := la
			t := now
			cp.UnusedSince = &t
			updates = append(updates, cp)
		case now.Sub(la.UnusedSince.Time) > 24*time.Hour:
			drops = append(drops, la.Key)
		}
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Key < updates[j].Key })
	sort.Strings(drops)
	return
}
