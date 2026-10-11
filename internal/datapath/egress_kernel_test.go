package datapath

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// TestEgressOnRealKernel sends real traffic from a pod through an Egress. Four
// network namespaces stand in for the pod, the pod's node, the egress node and
// a site on the internet. The pod's node and the egress node are joined by an
// underlay at 1350 bytes carrying a kuport link at 1300, and the pod's node
// has a second uplink of its own, its home address, that a masquerade rule in
// the CNI's place writes onto anything leaving by it.
func TestEgressOnRealKernel(t *testing.T) {
	inNetns(t, "TestEgressOnRealKernel", egressOnRealKernel)
}

var (
	egPod       = netip.MustParseAddr("10.0.0.2")
	egPodGw     = netip.MustParseAddr("10.0.0.1")
	egPod6      = netip.MustParseAddr("fd00::2")
	egPodGw6    = netip.MustParseAddr("fd00::1")
	egUnderA    = netip.MustParseAddr("192.168.50.1") // the pod's node
	egUnderB    = netip.MustParseAddr("192.168.50.2") // the egress node
	egPublic    = netip.MustParseAddr("198.51.100.10")
	egPublic6   = netip.MustParseAddr("2001:db8::10")
	egSite      = netip.MustParseAddr("198.51.100.50")
	egSite6     = netip.MustParseAddr("2001:db8::50")
	egFar       = netip.MustParseAddr("9.9.9.9") // below every cluster range
	egHome      = netip.MustParseAddr("203.0.113.1")
	egHomeGw    = netip.MustParseAddr("203.0.113.50")
	egHome6     = netip.MustParseAddr("2001:db8:1::1")
	egHomeGw6   = netip.MustParseAddr("2001:db8:1::50")
	egLinkA     = netip.MustParsePrefix("169.254.64.6/31")
	egLinkB     = netip.MustParsePrefix("169.254.64.7/31")
	egLinkA6    = netip.MustParsePrefix("fd64:f5ac:e961::1:6/127")
	egLinkB6    = netip.MustParsePrefix("fd64:f5ac:e961::1:7/127")
	egMark      = uint32(0x6b700003)
	egTable     = uint32(10003)
	egSitePort  = uint16(9000)
	egFarPort   = uint16(9002)
	egMappedUDP = uint16(40000)
)

func egressOnRealKernel(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	podNode, err := netns.Get()
	if err != nil {
		t.Fatalf("pod node netns: %v", err)
	}
	pod := newNetns(t, podNode)
	egress := newNetns(t, podNode)
	site := newNetns(t, podNode)

	// The pod's node.
	setUp(t, "lo")
	vethInto(t, "cilium_host", "p0", pod)
	vethInto(t, "u0", "u1", egress)
	vethInto(t, "home0", "s1", site)
	setMTU(t, "cilium_host", 1350)
	setMTU(t, "u0", 1350)
	addrOn(t, "cilium_host", egPodGw, 24)
	addrOn(t, "cilium_host", egPodGw6, 64)
	addrOn(t, "u0", egUnderA, 24)
	addrOn(t, "home0", egHome, 24)
	addrOn(t, "home0", egHome6, 64)
	defaultVia(t, "home0", egHomeGw)
	defaultVia(t, "home0", egHomeGw6)
	routerSysctls(t)
	cniMasquerade(t, "home0")

	inNs(t, pod, func() {
		setUp(t, "lo")
		setUp(t, "p0")
		setMTU(t, "p0", 1350)
		addrOn(t, "p0", egPod, 24)
		addrOn(t, "p0", egPod6, 64)
		defaultVia(t, "p0", egPodGw)
		defaultVia(t, "p0", egPodGw6)
	})

	inNs(t, egress, func() {
		setUp(t, "lo")
		setUp(t, "u1")
		setMTU(t, "u1", 1350)
		addrOn(t, "u1", egUnderB, 24)
		vethInto(t, "up0", "s0", site)
		addrOn(t, "up0", egPublic, 24)
		addrOn(t, "up0", egPublic6, 64)
		routeVia(t, "up0", netip.PrefixFrom(egFar, 32), egSite)
		routerSysctls(t)
	})

	inNs(t, site, func() {
		setUp(t, "lo")
		setUp(t, "s0")
		setUp(t, "s1")
		addrOn(t, "s0", egSite, 24)
		addrOn(t, "s0", egSite6, 64)
		addrOn(t, "s0", egFar, 32)
		addrOn(t, "s1", egHomeGw, 24)
		addrOn(t, "s1", egHomeGw6, 64)
		reportSource(t, egSitePort)
		reportSource(t, egMappedUDP+1)
		// A wildcard socket answers from the address routing picks for the
		// reply, which for a request to 9.9.9.9 is s0's primary address and
		// matches no flow on the egress node. Bound to 9.9.9.9, it answers
		// from the address dialed.
		reportSourceAt(t, netip.AddrPortFrom(egFar, egFarPort))
	})

	// A service inside the cluster, on the pod's node, for the pod to reach
	// without the egress node.
	reportSourceAt(t, netip.AddrPortFrom(egPodGw, egSitePort))

	// The pod's node routes a marked flow after every other policy rule: a
	// destination with a specific route in main keeps it, and only what
	// would take main's default route reaches the link's table, or the
	// blackhole while the Egress is blocked.
	egressRulesFor := func(mark, table uint32) []IPRule {
		return []IPRule{
			{Pref: 32763, Mark: mark, To: prefixOf(egUnderB, 32), Table: 254},
			{Pref: 32764, Mark: mark, Table: 254, SuppressDefault: true},
			{Pref: 32764, Mark: mark, Table: 254, SuppressDefault: true, Family: FamilyIPv6},
			{Pref: 32765, Mark: mark, Table: table},
			{Pref: 32765, Mark: mark, Table: table, Family: FamilyIPv6},
		}
	}
	podNodeState := func(egressRules []EgressRule) State {
		rules := append(egressRulesFor(egMark, egTable), egressRulesFor(EgressBlockedMark, EgressBlockedTable)...)
		return State{
			Exempt: []ExemptRule{
				{OifName: "kup-3", SrcAddr: addrPtrOf(egPod)},
				{OifName: "kup-3", SrcAddr: addrPtrOf(egPod6)},
			},
			Egress: egressRules,
			ClusterCIDRs: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/24"),
				netip.MustParsePrefix("fd00::/64"),
				netip.PrefixFrom(egUnderA, 32),
				netip.PrefixFrom(egUnderB, 32),
			},
			Links: []Link{{
				Name: "kup-3", VNI: 7003, Port: 4790, MTU: 1300,
				LocalAddr: egUnderA, RemoteAddr: egUnderB,
				LinkAddr: egLinkA, LinkAddr6: egLinkA6,
			}},
			Rules: rules,
			Routes: []Route{
				{Table: egTable, Via: egLinkB.Addr(), Dev: "kup-3"},
				{Table: egTable, Via: egLinkB6.Addr(), Dev: "kup-3"},
				{Table: EgressBlockedTable, Blackhole: true},
				{Table: EgressBlockedTable, Blackhole: true, BlackholeFamily: FamilyIPv6},
			},
		}
	}
	routed := []EgressRule{{SrcAddr: egPod, Mark: egMark}, {SrcAddr: egPod6, Mark: egMark}}
	blocked := []EgressRule{{SrcAddr: egPod, Mark: EgressBlockedMark}, {SrcAddr: egPod6, Mark: EgressBlockedMark}}

	// specificRoute checks that a destination main routes more specifically
	// than its default, here the site's addresses on the home link, leaves
	// from the pod's own node whatever the Egress does.
	specificRoute := func(t *testing.T) {
		t.Helper()
		for _, c := range []struct{ to, want netip.Addr }{
			{egHomeGw, egHome},
			{egHomeGw6, egHome6},
		} {
			if seen, err := podUDP(t, pod, 0, netip.AddrPortFrom(c.to, egSitePort)); err != nil {
				t.Errorf("udp to %s: %v", c.to, err)
			} else if seen != c.want {
				t.Errorf("udp to %s: the site saw %s, want the home address %s: a more specific route was overridden", c.to, seen, c.want)
			}
		}
	}

	apply := func(st State) {
		t.Helper()
		conn, err := nftables.New()
		if err != nil {
			t.Fatalf("nftables: %v", err)
		}
		if err := Apply(context.Background(), Render(st), Handles{NFT: conn, NL: realNetlink{}}); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	apply(podNodeState(routed))
	inNs(t, egress, func() {
		// The egress node also serves a PortMap on the mapped UDP port, so
		// kup-steer rewrites what arrives on up0 for that port.
		apply(State{
			DNAT: []DNATRule{
				{Iface: "up0", Port: PortSel{Proto: "udp", First: egMappedUDP, Last: egMappedUDP}, ToAddr: egLinkA.Addr()},
				{Iface: "up0", Port: PortSel{Proto: "udp", First: egMappedUDP, Last: egMappedUDP}, ToAddr: egLinkA6.Addr()},
			},
			EgressGateway: []EgressGatewayRule{{Iface: "kup-3", Mark: egMark}},
			Links: []Link{{
				Name: "kup-3", VNI: 7003, Port: 4790, MTU: 1300,
				LocalAddr: egUnderB, RemoteAddr: egUnderA,
				LinkAddr: egLinkB, LinkAddr6: egLinkB6,
			}},
			Rules: []IPRule{
				{Pref: 101, Mark: egMark, To: prefixOf(egUnderA, 32), Table: 254},
				{Pref: 102, Mark: egMark, Table: egTable},
				{Pref: 102, Mark: egMark, Table: egTable, Family: FamilyIPv6},
			},
			Routes: []Route{
				{Table: egTable, Via: egLinkA.Addr(), Dev: "kup-3"},
				{Table: egTable, Via: egLinkA6.Addr(), Dev: "kup-3"},
			},
			LocalAddrs: []netip.Addr{egPublic, egPublic6, egUnderB},
		})
	})
	rpFilterOff(t)
	inNs(t, egress, func() { rpFilterOff(t) })

	t.Run("udp leaves from the egress node", func(t *testing.T) {
		for _, c := range []struct {
			to   netip.AddrPort
			want netip.Addr
		}{
			{netip.AddrPortFrom(egSite, egSitePort), egPublic},
			{netip.AddrPortFrom(egSite6, egSitePort), egPublic6},
			// Below the cluster set's first range, where an interval set
			// built without a leading end element could misread it.
			{netip.AddrPortFrom(egFar, egFarPort), egPublic},
		} {
			if seen, err := podUDP(t, pod, 0, c.to); err != nil {
				t.Errorf("udp to %s: %v", c.to, err)
				dumpCounters(t, "pod node")
				inNs(t, egress, func() { dumpCounters(t, "egress node") })
			} else if seen != c.want {
				t.Errorf("udp to %s: the site saw %s, want %s", c.to, seen, c.want)
			}
		}
	})

	t.Run("a more specific route keeps its path", specificRoute)

	t.Run("a cluster destination stays on the pod's node", func(t *testing.T) {
		if seen, err := podUDP(t, pod, 0, netip.AddrPortFrom(egPodGw, egSitePort)); err != nil {
			t.Errorf("udp to %s: %v", egPodGw, err)
		} else if seen != egPod {
			t.Errorf("udp to %s: seen from %s, want the pod's own %s", egPodGw, seen, egPod)
		}
	})

	t.Run("tcp leaves from the egress node with its MSS clamped", func(t *testing.T) {
		// The link's 1300 less the IP and TCP headers is the clamped MSS, and
		// TCP_MAXSEG reports it less the 12 bytes of the timestamp option both
		// ends negotiate. Unclamped, the pod's 1350 would give 1298 and 1278.
		for _, c := range []struct {
			to, want netip.Addr
			mss      int
		}{
			{egSite, egPublic, 1300 - 40 - 12},
			{egSite6, egPublic6, 1300 - 60 - 12},
		} {
			seen, mss, n, err := podTCP(t, pod, netip.AddrPortFrom(c.to, egSitePort))
			switch {
			case err != nil:
				t.Errorf("tcp to %s: %v", c.to, err)
			case seen != c.want:
				t.Errorf("tcp to %s: the site saw %s, want %s", c.to, seen, c.want)
			case mss != c.mss:
				t.Errorf("tcp to %s: the site's MSS is %d, want %d from the link's 1300", c.to, mss, c.mss)
			case n != bulkBytes:
				t.Errorf("tcp to %s: read %d bytes of the site's bulk reply, want %d", c.to, n, bulkBytes)
			}
		}
	})

	t.Run("a reply on the mapped port reaches the pod through kup-steer", func(t *testing.T) {
		for _, c := range []struct{ to, want netip.Addr }{
			{egSite, egPublic},
			{egSite6, egPublic6},
		} {
			to := netip.AddrPortFrom(c.to, egMappedUDP+1)
			if seen, err := podUDP(t, pod, egMappedUDP, to); err != nil {
				t.Errorf("udp from :%d to %s: %v", egMappedUDP, to, err)
			} else if seen != c.want {
				t.Errorf("udp from :%d to %s: the site saw %s, want %s", egMappedUDP, to, seen, c.want)
			}
		}
	})

	t.Run("fail closed drops what would have left", func(t *testing.T) {
		apply(podNodeState(blocked))
		for _, to := range []netip.Addr{egSite, egSite6} {
			if seen, err := podUDP(t, pod, 0, netip.AddrPortFrom(to, egSitePort)); err == nil {
				t.Errorf("udp to %s got through, seen from %s; want it dropped", to, seen)
			}
		}
		if seen, err := podUDP(t, pod, 0, netip.AddrPortFrom(egPodGw, egSitePort)); err != nil || seen != egPod {
			t.Errorf("udp to the cluster while blocked: seen %s, err %v; want it delivered from %s", seen, err, egPod)
		}
		specificRoute(t)
	})

	t.Run("direct leaves from the pod's own node", func(t *testing.T) {
		apply(podNodeState(nil))
		// The site's addresses on the home side, so its wildcard socket
		// answers from the address the pod dialed.
		for _, c := range []struct{ to, want netip.Addr }{
			{egHomeGw, egHome},
			{egHomeGw6, egHome6},
		} {
			if seen, err := podUDP(t, pod, 0, netip.AddrPortFrom(c.to, egSitePort)); err != nil {
				t.Errorf("udp to %s: %v", c.to, err)
				dumpCounters(t, "pod node")
			} else if seen != c.want {
				t.Errorf("udp to %s: the site saw %s, want the home address %s", c.to, seen, c.want)
			}
		}
	})
}

// dumpCounters logs every rule's packet counter in the current netns, so a
// failing exchange shows which rule a packet reached.
func dumpCounters(t *testing.T, label string) {
	t.Helper()
	conn, err := nftables.New()
	if err != nil {
		t.Logf("%s: nftables: %v", label, err)
		return
	}
	chains, err := conn.ListChains()
	if err != nil {
		t.Logf("%s: chains: %v", label, err)
		return
	}
	for _, c := range chains {
		rules, err := conn.GetRules(c.Table, c)
		if err != nil {
			continue
		}
		for i, r := range rules {
			for _, e := range r.Exprs {
				if ctr, ok := e.(*expr.Counter); ok {
					t.Logf("%s: %s/%s rule %d: %d packets", label, c.Table.Name, c.Name, i, ctr.Packets)
				}
			}
		}
	}
}

// bulkBytes is what the site writes after its report line, so a TCP test moves
// full-size segments from the site back through the egress node and the link.
const bulkBytes = 200_000

// reportSource answers on every address the namespace holds: UDP with the
// source address it saw, TCP with that and its MSS for the connection,
// followed by bulkBytes of filler.
func reportSource(t *testing.T, port uint16) {
	t.Helper()
	reportSourceAt(t, netip.AddrPortFrom(netip.IPv6Unspecified(), port))
}

func reportSourceAt(t *testing.T, at netip.AddrPort) {
	t.Helper()
	u, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(at))
	if err != nil {
		t.Fatalf("udp listen %s: %v", at, err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			_, from, err := u.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			u.WriteToUDPAddrPort([]byte(from.Addr().Unmap().String()), from) //nolint:errcheck
		}
	}()
	l, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(at))
	if err != nil {
		t.Fatalf("tcp listen %s: %v", at, err)
	}
	go func() {
		for {
			c, err := l.AcceptTCP()
			if err != nil {
				return
			}
			go func() {
				defer c.Close() //nolint:errcheck
				from := netip.MustParseAddrPort(c.RemoteAddr().String()).Addr().Unmap()
				fmt.Fprintf(c, "%s %d\n", from, tcpMSS(c)) //nolint:errcheck
				c.Write(make([]byte, bulkBytes))           //nolint:errcheck
			}()
		}
	}()
}

func tcpMSS(c *net.TCPConn) int {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1
	}
	mss := -1
	raw.Control(func(fd uintptr) { //nolint:errcheck
		mss, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_MAXSEG)
	})
	return mss
}

// podUDP sends from the pod, from srcPort when it is not 0, and returns the
// source address the far end reported.
func podUDP(t *testing.T, pod netns.NsHandle, srcPort uint16, to netip.AddrPort) (netip.Addr, error) {
	t.Helper()
	var seen netip.Addr
	var rerr error
	inNs(t, pod, func() {
		local := &net.UDPAddr{Port: int(srcPort)}
		c, err := net.ListenUDP("udp", local)
		if err != nil {
			rerr = err
			return
		}
		defer c.Close() //nolint:errcheck
		buf := make([]byte, 1500)
		for try := 0; try < 3; try++ {
			if _, err := c.WriteToUDPAddrPort([]byte("who"), to); err != nil {
				rerr = err
				return
			}
			c.SetReadDeadline(time.Now().Add(time.Second)) //nolint:errcheck
			n, _, err := c.ReadFromUDPAddrPort(buf)
			if err == nil {
				seen, rerr = netip.ParseAddr(string(buf[:n]))
				return
			}
			rerr = err
		}
	})
	return seen, rerr
}

// podTCP connects from the pod and returns the source address and MSS the far
// end reported, and how many bytes of filler followed.
func podTCP(t *testing.T, pod netns.NsHandle, to netip.AddrPort) (netip.Addr, int, int, error) {
	t.Helper()
	var seen netip.Addr
	var mss, n int
	var rerr error
	inNs(t, pod, func() {
		c, err := net.DialTimeout("tcp", to.String(), 3*time.Second)
		if err != nil {
			rerr = err
			return
		}
		defer c.Close()                                    //nolint:errcheck
		c.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		b, err := io.ReadAll(c)
		if err != nil {
			rerr = err
			return
		}
		line, rest, ok := strings.Cut(string(b), "\n")
		if !ok {
			rerr = fmt.Errorf("no report line in %d bytes", len(b))
			return
		}
		var addr string
		if _, err := fmt.Sscanf(line, "%s %d", &addr, &mss); err != nil {
			rerr = fmt.Errorf("report %q: %w", line, err)
			return
		}
		seen, rerr = netip.ParseAddr(addr)
		n = len(rest)
	})
	return seen, mss, n, rerr
}

// cniMasquerade stands in for the CNI's masquerade: anything leaving by oif
// takes that interface's address, at the srcnat priority after kup-post.
func cniMasquerade(t *testing.T, oif string) {
	t.Helper()
	conn, err := nftables.New()
	if err != nil {
		t.Fatalf("nftables: %v", err)
	}
	accept := nftables.ChainPolicyAccept
	tbl := conn.AddTable(&nftables.Table{Name: "cni", Family: nftables.TableFamilyINet})
	ch := conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    tbl,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Policy:   &accept,
	})
	conn.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		ifnameCmp(oif, false),
		&expr.Counter{},
		&expr.Masq{},
	}})
	if err := conn.Flush(); err != nil {
		t.Fatalf("cni masquerade: %v", err)
	}
}

// routerSysctls turns on forwarding in both families in the current netns.
func routerSysctls(t *testing.T) {
	t.Helper()
	writeSysctl(t, "/proc/sys/net/ipv4/ip_forward")
	writeSysctl(t, "/proc/sys/net/ipv6/conf/all/forwarding")
}

// rpFilterOff sets rp_filter to 0 on every interface of the current netns, as
// measured on the cluster's workers. A reply arriving on a link carries a
// source the main table routes elsewhere, which strict mode would drop.
func rpFilterOff(t *testing.T) {
	t.Helper()
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	names := []string{"all", "default"}
	for _, l := range links {
		names = append(names, l.Attrs().Name)
	}
	for _, n := range names {
		if err := os.WriteFile("/proc/sys/net/ipv4/conf/"+n+"/rp_filter", []byte("0"), 0o644); err != nil {
			t.Fatalf("rp_filter on %s: %v", n, err)
		}
	}
}

func setMTU(t *testing.T, name string, mtu int) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("link %s: %v", name, err)
	}
	if err := netlink.LinkSetMTU(l, mtu); err != nil {
		t.Fatalf("mtu %d on %s: %v", mtu, name, err)
	}
}

func routeVia(t *testing.T, name string, dst netip.Prefix, gw netip.Addr) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("link %s: %v", name, err)
	}
	ipn := &net.IPNet{IP: dst.Addr().AsSlice(), Mask: net.CIDRMask(dst.Bits(), dst.Addr().BitLen())}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Dst: ipn, Gw: gw.AsSlice()}); err != nil {
		t.Fatalf("route %s via %s: %v", dst, gw, err)
	}
}

func addrPtrOf(a netip.Addr) *netip.Addr { return &a }

func prefixOf(a netip.Addr, bits int) *netip.Prefix {
	p := netip.PrefixFrom(a, bits)
	return &p
}
