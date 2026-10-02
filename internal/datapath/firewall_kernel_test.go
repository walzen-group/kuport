package datapath

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// TestDeliversThroughHostFirewallOnRealKernel sends real traffic through a
// mapping on a node whose host firewall drops every new connection to the
// node's own address, the way Talos's ingress firewall does from a filter
// chain at prerouting priority -110. Three network namespaces stand in for the
// client, the node and the pod. The mapping has to deliver UDP on the mapped
// port and TCP with a translated port, in IPv4 and IPv6, and every reply has to
// come back from the address and port the client dialed.
func TestDeliversThroughHostFirewallOnRealKernel(t *testing.T) {
	inNetns(t, "TestDeliversThroughHostFirewallOnRealKernel", deliversThroughHostFirewall)
}

// The addresses of the three namespaces. pub and pub6 are the node's uplink
// addresses, the ones a class publishes and the firewall protects.
var (
	fwPub     = netip.MustParseAddr("192.0.2.1")
	fwClient  = netip.MustParseAddr("192.0.2.2")
	fwPodGw   = netip.MustParseAddr("10.0.0.1")
	fwPod     = netip.MustParseAddr("10.0.0.2")
	fwPub6    = netip.MustParseAddr("2001:db8::1")
	fwClient6 = netip.MustParseAddr("2001:db8::2")
	fwPodGw6  = netip.MustParseAddr("fd00::1")
	fwPod6    = netip.MustParseAddr("fd00::2")
)

const (
	fwUDPPort    = 9987  // mapped and served on the same port
	fwTCPPort    = 30033 // mapped port
	fwTCPPodPort = 8030  // the pod's port behind it
)

func deliversThroughHostFirewall(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	node, err := netns.Get()
	if err != nil {
		t.Fatalf("node netns: %v", err)
	}
	client := newNetns(t, node)
	pod := newNetns(t, node)

	nl := realNetlink{}
	setUp(t, "lo")
	vethInto(t, "up0", "c0", client)
	vethInto(t, "cilium_host", "p0", pod)
	addrOn(t, "up0", fwPub, 24)
	addrOn(t, "up0", fwPub6, 64)
	addrOn(t, "cilium_host", fwPodGw, 24)
	addrOn(t, "cilium_host", fwPodGw6, 64)
	writeSysctl(t, "/proc/sys/net/ipv4/ip_forward")
	writeSysctl(t, "/proc/sys/net/ipv6/conf/all/forwarding")

	inNs(t, client, func() {
		setUp(t, "lo")
		setUp(t, "c0")
		addrOn(t, "c0", fwClient, 24)
		addrOn(t, "c0", fwClient6, 64)
	})
	inNs(t, pod, func() {
		setUp(t, "lo")
		setUp(t, "p0")
		addrOn(t, "p0", fwPod, 24)
		addrOn(t, "p0", fwPod6, 64)
		defaultVia(t, "p0", fwPodGw)
		defaultVia(t, "p0", fwPodGw6)
	})

	conn, err := nftables.New()
	if err != nil {
		t.Fatalf("nftables: %v", err)
	}

	// The state an accepting node's agent computes: DNAT on the interface for
	// any destination, and every address the node holds.
	state := State{
		DNAT: []DNATRule{
			{Iface: "up0", Port: PortSel{Proto: "udp", First: fwUDPPort, Last: fwUDPPort}, ToAddr: fwPod},
			{Iface: "up0", Port: PortSel{Proto: "udp", First: fwUDPPort, Last: fwUDPPort}, ToAddr: fwPod6},
			{Iface: "up0", Port: PortSel{Proto: "tcp", First: fwTCPPort, Last: fwTCPPort}, ToAddr: fwPod, ToPort: fwTCPPodPort},
			{Iface: "up0", Port: PortSel{Proto: "tcp", First: fwTCPPort, Last: fwTCPPort}, ToAddr: fwPod6, ToPort: fwTCPPodPort},
		},
		LocalAddrs: []netip.Addr{fwPub, fwPub6, fwPodGw, fwPodGw6},
	}
	if err := Apply(context.Background(), Render(state), Handles{NFT: conn, NL: nl}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	inNs(t, pod, func() {
		udpEcho(t, netip.AddrPortFrom(fwPod, fwUDPPort))
		udpEcho(t, netip.AddrPortFrom(fwPod6, fwUDPPort))
		tcpPong(t, netip.AddrPortFrom(fwPod, fwTCPPodPort))
		tcpPong(t, netip.AddrPortFrom(fwPod6, fwTCPPodPort))
	})

	// The same exchanges pass without the firewall, so a failure after it is
	// the firewall's doing and not the namespaces'.
	exchangeAll(t, client, "without the host firewall")
	hostFirewall(t, conn)
	exchangeAll(t, client, "behind the host firewall")
	meshFilter(t, conn)
	exchangeAll(t, client, "behind the host firewall and the mesh's forward filter")
}

// meshFilter writes the two netbird rules a mapping on wt0 passes through: at
// mangle priority it marks a packet from the interface whose destination is a
// local address, and its forward filter drops every packet from the interface
// that carries no mark. A rewrite of the destination ahead of the mark rule
// leaves the packet unmarked and the forward filter drops it.
func meshFilter(t *testing.T, conn *nftables.Conn) {
	t.Helper()
	const mark = 0x1bd20
	accept := nftables.ChainPolicyAccept
	tbl := conn.AddTable(&nftables.Table{Name: "mesh", Family: nftables.TableFamilyINet})
	pre := conn.AddChain(&nftables.Chain{
		Name:     "mangle-prerouting",
		Table:    tbl,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
		Policy:   &accept,
	})
	fwd := conn.AddChain(&nftables.Chain{
		Name:     "forward-filter",
		Table:    tbl,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &accept,
	})
	iif := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		ifnameCmp("up0", false),
	}
	conn.AddRule(&nftables.Rule{Table: tbl, Chain: pre, Exprs: append(append([]expr.Any{}, iif...),
		&expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(unix.RTN_LOCAL)},
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	)})
	conn.AddRule(&nftables.Rule{Table: tbl, Chain: fwd, Exprs: append(append([]expr.Any{}, iif...),
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark)},
		&expr.Counter{},
		&expr.Verdict{Kind: expr.VerdictDrop},
	)})
	if err := conn.Flush(); err != nil {
		t.Fatalf("mesh filter: %v", err)
	}
}

// exchangeAll dials every mapping from the client namespace and checks the
// reply's payload and, for UDP, the address it came from.
func exchangeAll(t *testing.T, client netns.NsHandle, label string) {
	t.Helper()
	for _, dialed := range []netip.AddrPort{
		netip.AddrPortFrom(fwPub, fwUDPPort),
		netip.AddrPortFrom(fwPub6, fwUDPPort),
	} {
		var from netip.AddrPort
		var got string
		var rerr error
		inNs(t, client, func() { from, got, rerr = udpExchange(dialed) })
		switch {
		case rerr != nil:
			t.Errorf("%s, udp to %s: %v", label, dialed, rerr)
		case from != dialed || got != "ping":
			t.Errorf("%s, udp to %s: reply %q from %s, want %q from %s", label, dialed, got, from, "ping", dialed)
		}
	}
	for _, dialed := range []netip.AddrPort{
		netip.AddrPortFrom(fwPub, fwTCPPort),
		netip.AddrPortFrom(fwPub6, fwTCPPort),
	} {
		var got string
		var rerr error
		inNs(t, client, func() { got, rerr = tcpExchange(dialed) })
		switch {
		case rerr != nil:
			t.Errorf("%s, tcp to %s: %v", label, dialed, rerr)
		case got != "pong":
			t.Errorf("%s, tcp to %s: read %q, want %q", label, dialed, got, "pong")
		}
	}
}

// hostFirewall writes the part of Talos's ingress firewall that matters here:
// a filter chain at prerouting priority -110 that drops a new connection to one
// of the node's own addresses.
func hostFirewall(t *testing.T, conn *nftables.Conn) {
	t.Helper()
	accept := nftables.ChainPolicyAccept
	tbl := conn.AddTable(&nftables.Table{Name: "talos", Family: nftables.TableFamilyINet})
	ch := conn.AddChain(&nftables.Chain{
		Name:     "prerouting",
		Table:    tbl,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityRef(-110),
		Policy:   &accept,
	})
	for _, a := range []netip.Addr{fwPub, fwPub6} {
		e := append([]expr.Any{
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{nfproto(familyOf(a))}},
		}, ipCmp(a, false)...)
		e = append(e,
			&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitNEW),
				Xor:            binaryutil.NativeEndian.PutUint32(0),
			},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(0)},
			&expr.Counter{},
			&expr.Verdict{Kind: expr.VerdictDrop},
		)
		conn.AddRule(&nftables.Rule{Table: tbl, Chain: ch, Exprs: e})
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("host firewall: %v", err)
	}
}

// newNetns creates a network namespace and returns the thread to back.
func newNetns(t *testing.T, back netns.NsHandle) netns.NsHandle {
	t.Helper()
	ns, err := netns.New()
	if err != nil {
		t.Fatalf("new netns: %v", err)
	}
	if err := netns.Set(back); err != nil {
		t.Fatalf("back to node netns: %v", err)
	}
	return ns
}

// inNs runs fn on a thread inside ns. A socket opened there stays in ns after
// fn returns.
func inNs(t *testing.T, ns netns.NsHandle, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		orig, err := netns.Get()
		if err != nil {
			t.Errorf("current netns: %v", err)
			return
		}
		defer orig.Close() //nolint:errcheck
		if err := netns.Set(ns); err != nil {
			t.Errorf("enter netns: %v", err)
			return
		}
		defer netns.Set(orig) //nolint:errcheck
		fn()
	}()
	<-done
}

func setUp(t *testing.T, name string) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("link %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(l); err != nil {
		t.Fatalf("up %s: %v", name, err)
	}
}

// vethInto creates a veth pair, moves the peer into ns and brings the local
// end up.
func vethInto(t *testing.T, local, peer string, ns netns.NsHandle) {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: local}, PeerName: peer}); err != nil {
		t.Fatalf("veth %s: %v", local, err)
	}
	p, err := netlink.LinkByName(peer)
	if err != nil {
		t.Fatalf("veth peer %s: %v", peer, err)
	}
	if err := netlink.LinkSetNsFd(p, int(ns)); err != nil {
		t.Fatalf("move %s: %v", peer, err)
	}
	setUp(t, local)
}

// addrOn adds an address without duplicate address detection, so an IPv6
// address is usable at once.
func addrOn(t *testing.T, name string, a netip.Addr, bits int) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("link %s: %v", name, err)
	}
	ipn := &net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(bits, a.BitLen())}
	if err := netlink.AddrAdd(l, &netlink.Addr{IPNet: ipn, Flags: unix.IFA_F_NODAD}); err != nil {
		t.Fatalf("addr %s on %s: %v", a, name, err)
	}
}

func defaultVia(t *testing.T, name string, gw netip.Addr) {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("link %s: %v", name, err)
	}
	fam := unix.AF_INET
	if gw.Is6() {
		fam = unix.AF_INET6
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: gw.AsSlice(), Family: fam}); err != nil {
		t.Fatalf("default via %s: %v", gw, err)
	}
}

func writeSysctl(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil {
		t.Fatalf("sysctl %s: %v", path, err)
	}
}

// udpEcho answers every datagram on at with its own payload.
func udpEcho(t *testing.T, at netip.AddrPort) {
	t.Helper()
	c, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(at))
	if err != nil {
		t.Fatalf("udp listen %s: %v", at, err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			c.WriteToUDPAddrPort(buf[:n], from) //nolint:errcheck
		}
	}()
}

// tcpPong writes "pong" to every connection accepted on at.
func tcpPong(t *testing.T, at netip.AddrPort) {
	t.Helper()
	l, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(at))
	if err != nil {
		t.Fatalf("tcp listen %s: %v", at, err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("pong")) //nolint:errcheck
			c.Close()               //nolint:errcheck
		}
	}()
}

// udpExchange sends "ping" from an unconnected socket and returns the first
// reply with the address it came from, so a reply from the wrong source is
// reported as such instead of being filtered out by a connected socket. It
// sends up to three times, as a client would: the first IPv6 datagram on a
// fresh veth can be lost while neighbour discovery completes.
func udpExchange(to netip.AddrPort) (netip.AddrPort, string, error) {
	c, err := net.ListenUDP("udp", nil)
	if err != nil {
		return netip.AddrPort{}, "", err
	}
	defer c.Close() //nolint:errcheck
	buf := make([]byte, 1500)
	for try := 0; ; try++ {
		if _, err := c.WriteToUDPAddrPort([]byte("ping"), to); err != nil {
			return netip.AddrPort{}, "", err
		}
		c.SetReadDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err == nil {
			return netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), string(buf[:n]), nil
		}
		if try == 2 {
			return netip.AddrPort{}, "", err
		}
	}
}

func tcpExchange(to netip.AddrPort) (string, error) {
	c, err := net.DialTimeout("tcp", to.String(), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()                                    //nolint:errcheck
	c.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	b, err := io.ReadAll(c)
	return string(b), err
}
