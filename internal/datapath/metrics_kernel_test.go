package datapath

import (
	"context"
	"net/netip"
	"runtime"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netns"
)

// TestCountsMappedTrafficOnRealKernel sends real traffic through two mappings
// and reads kup-count back from the kernel. Each UDP exchange is one datagram
// each way, so five exchanges read five on the request rule and five on the
// reply rule. The TCP mapping translates its port, and its rules count on the
// pod's port. A client reaching the pod's address directly, with no DNAT on
// the way, leaves every counter as it was.
func TestCountsMappedTrafficOnRealKernel(t *testing.T) {
	inNetns(t, "TestCountsMappedTrafficOnRealKernel", countsMappedTraffic)
}

func countsMappedTraffic(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	node, err := netns.Get()
	if err != nil {
		t.Fatalf("node netns: %v", err)
	}
	client := newNetns(t, node)
	pod := newNetns(t, node)

	setUp(t, "lo")
	vethInto(t, "up0", "c0", client)
	vethInto(t, "cilium_host", "p0", pod)
	addrOn(t, "up0", fwPub, 24)
	addrOn(t, "cilium_host", fwPodGw, 24)
	writeSysctl(t, "/proc/sys/net/ipv4/ip_forward")

	inNs(t, client, func() {
		setUp(t, "lo")
		setUp(t, "c0")
		addrOn(t, "c0", fwClient, 24)
		routeVia(t, "c0", netip.MustParsePrefix("10.0.0.0/24"), fwPub)
	})
	inNs(t, pod, func() {
		setUp(t, "lo")
		setUp(t, "p0")
		addrOn(t, "p0", fwPod, 24)
		defaultVia(t, "p0", fwPodGw)
	})

	conn, err := nftables.New()
	if err != nil {
		t.Fatalf("nftables: %v", err)
	}
	state := State{
		DNAT: []DNATRule{
			{Iface: "up0", Port: PortSel{Proto: "udp", First: fwUDPPort, Last: fwUDPPort}, ToAddr: fwPod},
			{Iface: "up0", Port: PortSel{Proto: "tcp", First: fwTCPPort, Last: fwTCPPort}, ToAddr: fwPod, ToPort: fwTCPPodPort},
		},
		LocalAddrs: []netip.Addr{fwPub, fwPodGw},
	}
	plan := Render(state)
	if err := Apply(context.Background(), plan, Handles{NFT: conn, NL: realNetlink{}}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	inNs(t, pod, func() {
		udpEcho(t, netip.AddrPortFrom(fwPod, fwUDPPort))
		tcpPong(t, netip.AddrPortFrom(fwPod, fwTCPPodPort))
	})

	mapped := netip.AddrPortFrom(fwPub, fwUDPPort)
	for i := range 5 {
		var rerr error
		inNs(t, client, func() { _, _, rerr = udpExchange(mapped) })
		if rerr != nil {
			t.Fatalf("udp exchange %d to %s: %v", i, mapped, rerr)
		}
	}
	var got string
	var rerr error
	inNs(t, client, func() { got, rerr = tcpExchange(netip.AddrPortFrom(fwPub, fwTCPPort)) })
	if rerr != nil || got != "pong" {
		t.Fatalf("tcp exchange: read %q, %v", got, rerr)
	}

	// The pod's address dialed directly: same interface, address and port as
	// the UDP mapping's rules, and conntrack records no DNAT.
	direct := netip.AddrPortFrom(fwPod, fwUDPPort)
	for i := range 3 {
		var from netip.AddrPort
		inNs(t, client, func() { from, _, rerr = udpExchange(direct) })
		if rerr != nil || from != direct {
			t.Fatalf("direct udp exchange %d: reply from %s, %v", i, from, rerr)
		}
	}

	c := countRules(t, conn, plan)
	want := []struct {
		rule string
		min  uint64
		max  uint64
	}{
		{`iifname "up0" ip daddr 10.0.0.2 udp dport 9987 ct direction original ct status dnat counter`, 5, 5},
		{`oifname "up0" ip saddr 10.0.0.2 udp sport 9987 ct direction reply ct status dnat counter`, 5, 5},
		{`iifname "up0" ip daddr 10.0.0.2 tcp dport 8030 ct direction original ct status dnat counter`, 1, 1 << 20},
		{`oifname "up0" ip saddr 10.0.0.2 tcp sport 8030 ct direction reply ct status dnat counter`, 1, 1 << 20},
	}
	for _, w := range want {
		n, ok := c[w.rule]
		switch {
		case !ok:
			t.Errorf("kup-count has no rule %s; it has %v", w.rule, c)
		case n < w.min || n > w.max:
			t.Errorf("%s counted %d packets, want %d to %d", w.rule, n, w.min, w.max)
		}
	}

	// The plan holds no link, so kup-mss is empty, and ReadLive returns each
	// counted chain's own rules: as many as the plan put there.
	live, err := ReadLive(conn)
	if err != nil {
		t.Fatalf("read live counters: %v", err)
	}
	gotPer, wantPer := map[string]int{}, map[string]int{}
	for _, l := range live {
		gotPer[l.Chain]++
	}
	for _, r := range plan.Rules {
		wantPer[r.Chain]++
	}
	for _, ch := range liveChains {
		if gotPer[ch] != wantPer[ch] {
			t.Errorf("ReadLive returned %d rules for %s, the plan has %d", gotPer[ch], ch, wantPer[ch])
		}
	}

	// The agent folds the live counters before each Apply, which replaces the
	// table and starts every counter at zero. Three more exchanges after it,
	// and the totals hold all eight.
	counters := NewCounters(conn)
	if err := counters.FoldAndApply(func() error {
		return Apply(context.Background(), plan, Handles{NFT: conn, NL: realNetlink{}})
	}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if after := countRules(t, conn, plan)[`iifname "up0" ip daddr 10.0.0.2 udp dport 9987 ct direction original ct status dnat counter`]; after != 0 {
		t.Errorf("request counter after the second apply = %d, want 0 (a fresh table)", after)
	}
	for i := range 3 {
		inNs(t, client, func() { _, _, rerr = udpExchange(mapped) })
		if rerr != nil {
			t.Fatalf("udp exchange %d after the apply: %v", i, rerr)
		}
	}
	totals, err := counters.Read()
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	udp := PortSel{Proto: "udp", First: fwUDPPort, Last: fwUDPPort}
	// kup-pre's accepting rule matches any destination on up0, so the three
	// exchanges dialed straight to the pod are new flows there too. Their DNAT
	// to the address they already carry changes no tuple, conntrack sets no
	// dnat status, and kup-count leaves them out.
	for _, w := range []struct {
		name string
		key  LiveKey
		want uint64
	}{
		{"request", LiveKey{Chain: ChainCount, Iif: "up0", Family: FamilyIPv4, Proto: "udp", Port: udp, Dir: CtDirOriginal, Target: fwPod}, 8},
		{"reply", LiveKey{Chain: ChainCount, Oif: "up0", Family: FamilyIPv4, Proto: "udp", Port: udp, Dir: CtDirReply, Target: fwPod}, 8},
		{"new flows", LiveKey{Chain: ChainPre, Iif: "up0", Family: FamilyIPv4, Proto: "udp", Port: udp, Target: fwPod}, 11},
	} {
		if got := totals[w.key].Packets; got != w.want {
			t.Errorf("%s total = %d packets, want %d; totals %v", w.name, got, w.want, totals)
		}
	}
}

// countRules reads kup-count from the kernel and returns each rule's packet
// counter by its text. The kernel returns a chain's rules in order, which is
// the order Render gave them, so rule i is matched to the plan's rule i.
func countRules(t *testing.T, conn *nftables.Conn, plan Plan) map[string]uint64 {
	t.Helper()
	ch := &nftables.Chain{Name: ChainCount, Table: nftTable()}
	live, err := conn.GetRules(nftTable(), ch)
	if err != nil {
		t.Fatalf("rules of %s: %v", ChainCount, err)
	}
	var planned []Rule
	for _, r := range plan.Rules {
		if r.Chain == ChainCount {
			planned = append(planned, r)
		}
	}
	out := map[string]uint64{}
	for i, r := range live {
		if i >= len(planned) {
			t.Fatalf("kernel holds %d kup-count rules, plan has %d", len(live), len(planned))
		}
		for _, e := range r.Exprs {
			if ctr, ok := e.(*expr.Counter); ok {
				out[nftText(planned[i])] = ctr.Packets
			}
		}
	}
	return out
}
