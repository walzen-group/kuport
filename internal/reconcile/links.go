package reconcile

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// Every return link in the cluster takes its numbers from one pool of slots. A
// slot is claimed by creating the Link object named slot-<n>; the API server
// refuses a second create of that name, so no two links anywhere share a slot,
// whichever classes they belong to. docs/decisions.md records why the pool is
// cluster-wide.
const (
	// linkSlots is the size of the pool.
	linkSlots = 1024
	// linkSlotPrefix is the name prefix of every Link object.
	linkSlotPrefix = "slot-"
	// vniBase is the VNI of slot 0.
	vniBase = 7000
	// tableBase is the routing table of slot 0. Tables 253 to 255 belong to
	// the kernel, Cilium routes through 2004 and 2005 and netbird through
	// 7120, and the old per-class tables ran from 200.
	tableBase = 10000
	// defaultLinkPort is the links' UDP port when the agent sets none.
	defaultLinkPort = 4790
	// unusedLinkAge is how long a Link may sit unused before any agent
	// deletes it.
	unusedLinkAge = 24 * time.Hour
)

var (
	// linkSubnet holds a /31 per slot: 1024 of them in a /21.
	linkSubnet = netip.MustParsePrefix("169.254.64.0/21")
	// linkSubnet6 holds a /127 per slot. The 40 bits after fd are random, per
	// RFC 4193, the prefix the old per-class default used; ::1:0 keeps it
	// clear of that default's ::/112.
	linkSubnet6 = netip.MustParsePrefix("fd64:f5ac:e961::1:0/112")
)

// ipv6MinMTU is the smallest MTU a device may have and still carry IPv6. The
// kernel refuses an IPv6 address on a smaller device, and that refusal would
// fail the whole apply, IPv4 included.
const ipv6MinMTU = 1280

// markBase is OR'd with a slot to form the packet mark that keeps a return
// diversion narrow. The high bits spell "kp".
const markBase uint32 = 0x6b700000

// linkMark is the packet mark of the link at a slot. Cilium matches its own
// marks under the mask 0xf00, and its rule at pref 9 sends mark&0xf00 == 0x200
// to its proxy table, so a link's mark leaves those four bits clear: the slot's
// low eight bits stay in the low byte and the next two move to bits 12 and 13.
func linkMark(slot int) uint32 {
	s := uint32(slot)
	return markBase | s&0xff | (s>>8)<<12
}

// linkClass names the class a link belongs to.
type linkClass struct {
	kind string
	name string
}

// neededPair is a node pair a class needs a return link for, with the set of
// nodes that hold a chosen endpoint on it. Only a holder proposes a new Link.
type neededPair struct {
	class   linkClass
	peers   [2]string
	holders map[string]bool
}

// key identifies the pair within its class, the same way a Link's spec does.
func (p neededPair) key() string { return classLinkKey(p.class, p.peers) }

// classLinkKey is the class kind, the class name and the sorted peers, joined.
func classLinkKey(c linkClass, peers [2]string) string {
	return c.kind + "/" + c.name + "/" + peers[0] + "/" + peers[1]
}

// specKey is classLinkKey read off a Link object, and false for a malformed spec.
func specKey(l *v1alpha1.Link) (string, bool) {
	if len(l.Spec.Peers) != 2 {
		return "", false
	}
	p := sortedPair(l.Spec.Peers[0], l.Spec.Peers[1])
	return classLinkKey(linkClass{kind: l.Spec.ClassKind, name: l.Spec.ClassName}, p), true
}

// slotOf parses a Link's name into its slot, and false for a name outside the
// pool. Such an object is left alone.
func slotOf(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, linkSlotPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || n >= linkSlots || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// slotName is the Link object name for a slot.
func slotName(slot int) string { return linkSlotPrefix + strconv.Itoa(slot) }

// neededPairs collects the return-link pairs every accepted mapping needs,
// keyed by classLinkKey. A pair is needed for each programming node that sits off
// the chosen endpoint's node. Under Single that is at most one pair; under
// Multi it is one per accepting node that lacks the pod, since each of them
// forwards and each needs its own way back.
func neededPairs(mappings []*mapping) map[string]neededPair {
	out := map[string]neededPair{}
	for _, m := range mappings {
		if !m.accepted || m.endpoint == nil || m.effAccepting == "" || !m.remote {
			continue
		}
		cls := linkClass{kind: v1alpha1.ClassKindPortMapClass, name: m.pm.Spec.ClassName}
		for _, peer := range remoteProgrammers(m, m.endpoint.node) {
			addNeeded(out, cls, peer, m.endpoint.node)
		}
	}
	return out
}

// addNeeded records that holder needs a link of a class to peer.
func addNeeded(out map[string]neededPair, cls linkClass, peer, holder string) {
	np := neededPair{class: cls, peers: sortedPair(peer, holder)}
	key := np.key()
	if have, ok := out[key]; ok {
		np = have
	} else {
		np.holders = map[string]bool{}
	}
	np.holders[holder] = true
	out[key] = np
}

// heldLink is one Link object in the pool.
type heldLink struct {
	link *v1alpha1.Link
	slot int
}

// linkAlloc is the pool as of this pass: the Link each key uses, the Links
// that duplicate a key's lower slot, the slots newly picked for needed pairs
// that have none, and the classes a full pool left without one. A newly picked
// slot exists only to propose its Link; nothing that carries the slot into the
// host is built until the Link is in the cache.
type linkAlloc struct {
	byKey      map[string]heldLink
	duplicates map[string][]heldLink // key -> Links above the slot byKey uses
	needed     map[string]neededPair
	newly      map[string]int
	exhausted  map[linkClass]bool
	all        []heldLink
}

// allocateLinks settles the slot of every needed pair. A key with Links keeps
// the lowest of their slots. A needed key with none gets the lowest slot no
// Link holds, assigned in sorted key order so every agent computes the same
// proposal.
func allocateLinks(links []*v1alpha1.Link, needed map[string]neededPair) linkAlloc {
	a := linkAlloc{
		byKey:      map[string]heldLink{},
		duplicates: map[string][]heldLink{},
		needed:     needed,
		newly:      map[string]int{},
		exhausted:  map[linkClass]bool{},
	}
	used := map[int]bool{}
	for _, l := range links {
		slot, ok := slotOf(l.Name)
		if !ok {
			continue
		}
		used[slot] = true
		if _, ok := specKey(l); !ok {
			continue
		}
		a.all = append(a.all, heldLink{link: l, slot: slot})
	}
	sort.Slice(a.all, func(i, j int) bool { return a.all[i].slot < a.all[j].slot })
	for _, h := range a.all {
		key, _ := specKey(h.link)
		if _, ok := a.byKey[key]; ok {
			a.duplicates[key] = append(a.duplicates[key], h)
			continue
		}
		a.byKey[key] = h
	}

	keys := make([]string, 0, len(needed))
	for k := range needed {
		if _, ok := a.byKey[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		slot := lowestFree(used, linkSlots)
		if slot < 0 {
			a.exhausted[needed[k].class] = true
			continue
		}
		a.newly[k] = slot
		used[slot] = true
	}
	return a
}

// slotFor returns the slot of a class's link between two nodes once its Link
// is in the cache, and false while it is still being created. Marks, divert
// rules, links and routes all carry the slot; emitting any of them against a
// proposed slot lets a lost create race mark packets with a slot another link
// holds.
func (a linkAlloc) slotFor(cls linkClass, x, y string) (int, bool) {
	h, ok := a.byKey[classLinkKey(cls, sortedPair(x, y))]
	return h.slot, ok
}

// landedSlotFor is slotFor for one of a mapping's return links: the one between
// a programmer and the endpoint's node. It takes the peer rather than reading
// one off the mapping because Multi serving gives the pod's node a link per
// remote programmer, each on its own slot, so a reply can leave by the link its
// request arrived on.
func (a linkAlloc) landedSlotFor(m *mapping, peer string) (int, bool) {
	return a.slotFor(linkClass{kind: v1alpha1.ClassKindPortMapClass, name: m.pm.Spec.ClassName}, peer, m.endpoint.node)
}

// LinkStatusUpdate sets or clears one Link's UnusedSince. ResourceVersion is
// the version this pass read, so a write against a newer object conflicts and
// is recomputed.
type LinkStatusUpdate struct {
	Name            string
	ResourceVersion string
	UnusedSince     *metav1.Time
}

// LinkDelete removes one Link, on the condition that it is still the version
// this pass read.
type LinkDelete struct {
	Name            string
	ResourceVersion string
}

// LinkActions are the writes to the Link pool this node makes in one pass.
type LinkActions struct {
	Create []v1alpha1.Link
	Status []LinkStatusUpdate
	Delete []LinkDelete
}

// linkActions computes this node's writes to the pool. Every agent computes
// the same needed set, so every agent may set, clear or act on UnusedSince,
// and a Link whose nodes are both gone is still released. Only a pair's holder
// creates its Link and deletes the duplicates of it, since the holder is the
// one agent that proposes the pair.
func linkActions(a linkAlloc, nodeName string, now metav1.Time) LinkActions {
	var out LinkActions

	keys := make([]string, 0, len(a.newly))
	for k := range a.newly {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		np := a.needed[k]
		if !np.holders[nodeName] {
			continue
		}
		out.Create = append(out.Create, v1alpha1.Link{
			ObjectMeta: metav1.ObjectMeta{Name: slotName(a.newly[k])},
			Spec: v1alpha1.LinkSpec{
				ClassKind: np.class.kind,
				ClassName: np.class.name,
				Peers:     []string{np.peers[0], np.peers[1]},
			},
		})
	}

	for _, h := range a.all {
		key, _ := specKey(h.link)
		np, isNeeded := a.needed[key]
		isDuplicate := a.byKey[key].slot != h.slot
		since := h.link.Status.UnusedSince

		switch {
		case isNeeded && isDuplicate:
			if np.holders[nodeName] {
				out.Delete = append(out.Delete, LinkDelete{Name: h.link.Name, ResourceVersion: h.link.ResourceVersion})
			}
		case isNeeded && since != nil:
			out.Status = append(out.Status, LinkStatusUpdate{Name: h.link.Name, ResourceVersion: h.link.ResourceVersion})
		case !isNeeded && since == nil:
			t := now
			out.Status = append(out.Status, LinkStatusUpdate{Name: h.link.Name, ResourceVersion: h.link.ResourceVersion, UnusedSince: &t})
		case !isNeeded && now.Sub(since.Time) > unusedLinkAge:
			out.Delete = append(out.Delete, LinkDelete{Name: h.link.Name, ResourceVersion: h.link.ResourceVersion})
		}
	}
	return out
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
	// The IPv6 ends, set when the link is large enough for IPv6; has6 says
	// which.
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

// buildLinkParams derives the link for the pair (thisNode, peer) at a slot.
// underlay reads a node's reported underlay MTU from the class status. It
// returns false when either node's address cannot be resolved.
func buildLinkParams(idx *index, underlay func(node string) int, thisNode, peer string, slot int, port uint16) (linkParams, bool) {
	local, err1 := netip.ParseAddr(nodeAddrOf(idx, thisNode))
	remote, err2 := netip.ParseAddr(nodeAddrOf(idx, peer))
	if err1 != nil || err2 != nil {
		return linkParams{}, false
	}

	// The lower-named node gets the low address of each pair, in both
	// families, so both ends compute each other's addresses from the slot.
	first := thisNode == sortedPair(thisNode, peer)[0]
	ends := func(lo, hi netip.Addr) (netip.Addr, netip.Addr) {
		if first {
			return lo, hi
		}
		return hi, lo
	}
	thisEnd, peerEnd := ends(nthSlash31(linkSubnet, slot))

	// The kernel keys a VXLAN device by VNI and UDP port, and every link
	// shares the port, so the VNI carries the slot.
	lp := linkParams{
		slot:       slot,
		name:       linkName(slot),
		localAddr:  local,
		remoteAddr: remote,
		thisEnd:    thisEnd,
		peerEnd:    peerEnd,
		linkAddr:   netip.PrefixFrom(thisEnd, 31),
		peerNode32: netip.PrefixFrom(remote, 32),
		mark:       linkMark(slot),
		table:      tableBase + uint32(slot),
		vni:        vniBase + uint32(slot),
		port:       port,
		mtu:        linkMTUFor(underlay(thisNode), underlay(peer), local.Is4() && remote.Is4()),
	}

	// Both ends read the same two MTU rows, so they agree on whether the link
	// carries IPv6 as they agree on everything else.
	if lp.mtu == 0 || lp.mtu >= ipv6MinMTU {
		lp.has6 = true
		lp.thisEnd6, lp.peerEnd6 = ends(nthSlash127(linkSubnet6, slot))
		lp.linkAddr6 = netip.PrefixFrom(lp.thisEnd6, 127)
	}
	return lp, true
}

// linkPort is the links' UDP port, the agent's setting or the default.
func linkPort(in Inputs) uint16 {
	if in.LinkPort != 0 {
		return in.LinkPort
	}
	return defaultLinkPort
}

// linkMTUFor sizes the device to the path it rides: the smaller of the two
// nodes' underlays less the encapsulation. Each agent reads both figures from
// the class status, so the two ends agree without exchanging anything. A peer
// that has not reported yet leaves the link at this node's own figure, and the
// pass that follows the peer's first status write shrinks it.
func linkMTUFor(thisUnderlay, peerUnderlay int, outerIsV4 bool) uint32 {
	mtu := datapath.LinkMTUBetween(thisUnderlay, peerUnderlay, outerIsV4)
	if mtu <= 0 {
		return 0
	}
	return uint32(mtu)
}

// classUnderlay reads nodes' reported underlay MTUs from a PortMapClass's
// status, 0 for a node that has written no row.
func classUnderlay(class *v1alpha1.PortMapClass) func(string) int {
	return func(name string) int {
		if row := classNodeRow(class, name); row != nil {
			return int(row.UnderlayMTU)
		}
		return 0
	}
}

// deprecatedVxlanFields names the ignored returnPath.vxlan fields a class sets.
func deprecatedVxlanFields(class *v1alpha1.PortMapClass) []string {
	v := class.Spec.ReturnPath.Vxlan
	if v == nil {
		return nil
	}
	var out []string
	if v.VNI != 0 {
		out = append(out, "returnPath.vxlan.vni")
	}
	if v.Port != 0 {
		out = append(out, "returnPath.vxlan.port")
	}
	if v.Subnet != "" {
		out = append(out, "returnPath.vxlan.subnet")
	}
	if v.Subnet6 != "" {
		out = append(out, "returnPath.vxlan.subnet6")
	}
	return out
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

// linkName is the vxlan device name for the link at a slot. The slot is unique
// cluster-wide, so two classes wanting a link between the same two nodes get
// two devices, and both ends and every agent name each device the same.
func linkName(slot int) string {
	return fmt.Sprintf("kup-%d", slot)
}

// pairKey is the two node names sorted and joined with a slash.
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
