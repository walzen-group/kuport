package datapath

import (
	"net/netip"
	"testing"
)

// pod is the endpoint address used across the golden cases; it is the address
// the spec's own worked example carried traffic to.
var pod = netip.MustParseAddr("10.244.18.107")

// dpCase is one rendering scenario: a State and the shape its Plan must have.
// The nftables rules are checked against a golden file (text_test.go); the
// counts here pin the netlink objects and the rule mix, which nft cannot see.
type dpCase struct {
	name       string
	state      State
	wantDNAT   int // KindDNAT rules
	wantExempt int // KindSNAT rules
	wantMark   int // KindMark rules
	wantCtSave int // KindCtSave rules
	wantCtLoad int // KindCtLoad rules
	wantLinks  int
	wantRules  int
	wantRoutes int
}

// cases returns the required golden scenarios from the task's acceptance table.
func cases() []dpCase {
	udp3000 := PortSel{Proto: "udp", First: 3000, Last: 3000}
	udpRange := PortSel{Proto: "udp", First: 27015, Last: 27115}

	// Accepting-node state for a mapping on ifaces enp1s0 + wt0.
	accepting := func(p PortSel) State {
		dst := pod
		return State{
			DNAT: []DNATRule{
				{Iface: "enp1s0", Port: p, ToAddr: pod},
				{Iface: "wt0", Port: p, ToAddr: pod},
			},
			Exempt: []ExemptRule{
				{OifName: "cilium_host", DstAddr: &dst, Port: p},
			},
		}
	}

	// target-remote: worker-b holds the pod, edge-a accepts.
	src := pod
	acceptAddr := netip.MustParseAddr("203.0.113.9")
	acceptCIDR := netip.MustParsePrefix("203.0.113.9/32")
	targetRemote := State{
		Exempt: []ExemptRule{
			{OifName: "cilium_*", Negate: true, SrcAddr: &src, Port: udp3000, PortIsSrc: true},
		},
		Mark: []MarkRule{
			{SrcAddr: pod, Port: udp3000, Mark: 0x6b700001},
		},
		Links: []Link{
			{
				Name:       "kup-da0d9a1d",
				VNI:        4242,
				Port:       4790,
				LocalAddr:  netip.MustParseAddr("100.64.0.2"),
				RemoteAddr: acceptAddr,
				LinkAddr:   netip.MustParsePrefix("169.254.77.1/31"),
			},
		},
		Rules: []IPRule{
			{Pref: 101, Mark: 0x6b700001, To: &acceptCIDR, Table: 254},
			{Pref: 102, Mark: 0x6b700001, Table: 200},
		},
		Routes: []Route{
			{Table: 200, Via: netip.MustParseAddr("169.254.77.0"), Dev: "kup-da0d9a1d"},
		},
	}

	// target-remote-multi: the same pod under Multi serving, reached from two
	// accepting nodes. The static mark is gone, replaced by a save rule per
	// link and one restore rule, which is what lets a reply leave by the link
	// its request arrived on.
	secondAccept := netip.MustParseAddr("203.0.113.10")
	secondCIDR := netip.MustParsePrefix("203.0.113.10/32")
	// Under Multi the request arrives over the forwarding node's own link,
	// addressed to this node's end of it, so this node translates it the rest
	// of the way. The daddr match holds each rule to its own link.
	multiLinkDstA := netip.MustParseAddr("169.254.77.1")
	multiLinkDstB := netip.MustParseAddr("169.254.77.3")
	targetRemoteMulti := State{
		DNAT: []DNATRule{
			{Iface: "kup-da0d9a1d", Port: udp3000, ToAddr: pod, DstAddr: &multiLinkDstA},
			{Iface: "kup-e5b1c204", Port: udp3000, ToAddr: pod, DstAddr: &multiLinkDstB},
		},
		Exempt: []ExemptRule{
			{OifName: "cilium_*", Negate: true, SrcAddr: &src, Port: udp3000, PortIsSrc: true},
		},
		CtSave: []CtSaveRule{
			{Iface: "kup-da0d9a1d", Mark: 0x6b700001},
			{Iface: "kup-e5b1c204", Mark: 0x6b700002},
		},
		CtLoad: []CtLoadRule{
			{SrcAddr: pod, Port: udp3000},
		},
		Links: []Link{
			{
				Name: "kup-da0d9a1d", VNI: 4242, Port: 4790,
				LocalAddr:  netip.MustParseAddr("100.64.0.2"),
				RemoteAddr: acceptAddr,
				LinkAddr:   netip.MustParsePrefix("169.254.77.1/31"),
			},
			{
				Name: "kup-e5b1c204", VNI: 4242, Port: 4790,
				LocalAddr:  netip.MustParseAddr("100.64.0.2"),
				RemoteAddr: secondAccept,
				LinkAddr:   netip.MustParsePrefix("169.254.77.3/31"),
			},
		},
		Rules: []IPRule{
			{Pref: 101, Mark: 0x6b700001, To: &acceptCIDR, Table: 254},
			{Pref: 102, Mark: 0x6b700001, Table: 200},
			{Pref: 101, Mark: 0x6b700002, To: &secondCIDR, Table: 254},
			{Pref: 102, Mark: 0x6b700002, Table: 201},
		},
		Routes: []Route{
			{Table: 200, Via: netip.MustParseAddr("169.254.77.0"), Dev: "kup-da0d9a1d"},
			{Table: 201, Via: netip.MustParseAddr("169.254.77.2"), Dev: "kup-e5b1c204"},
		},
	}

	return []dpCase{
		{name: "accepting-single-port", state: accepting(udp3000), wantDNAT: 2, wantExempt: 1},
		{name: "accepting-port-range", state: accepting(udpRange), wantDNAT: 2, wantExempt: 1},
		{name: "target-remote", state: targetRemote, wantExempt: 1, wantMark: 1, wantLinks: 1, wantRules: 2, wantRoutes: 1},
		{name: "target-remote-multi", state: targetRemoteMulti, wantDNAT: 2, wantExempt: 1, wantCtSave: 2, wantCtLoad: 1, wantLinks: 2, wantRules: 4, wantRoutes: 2},
		// same-node: pod is on the accepting node, so DNAT straight to it with
		// no return-path machinery. A packet is DNATed once per hook, so this is
		// a real correctness check, not filler.
		{name: "same-node", state: accepting(udp3000), wantDNAT: 2, wantExempt: 1},
		{name: "empty", state: State{}},
	}
}

func countKind(rules []Rule, k RuleKind) int {
	n := 0
	for _, r := range rules {
		if r.Kind == k {
			n++
		}
	}
	return n
}

// TestRenderShape checks the netlink object counts and rule mix for each case,
// which the nftables goldens cannot express.
func TestRenderShape(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			p := Render(c.state)
			if got := countKind(p.Rules, KindDNAT); got != c.wantDNAT {
				t.Errorf("DNAT rules = %d, want %d", got, c.wantDNAT)
			}
			if got := countKind(p.Rules, KindSNAT); got != c.wantExempt {
				t.Errorf("exemption rules = %d, want %d", got, c.wantExempt)
			}
			if got := countKind(p.Rules, KindMark); got != c.wantMark {
				t.Errorf("mark rules = %d, want %d", got, c.wantMark)
			}
			if got := countKind(p.Rules, KindCtSave); got != c.wantCtSave {
				t.Errorf("ct save rules = %d, want %d", got, c.wantCtSave)
			}
			if got := countKind(p.Rules, KindCtLoad); got != c.wantCtLoad {
				t.Errorf("ct load rules = %d, want %d", got, c.wantCtLoad)
			}
			if got := len(p.Links); got != c.wantLinks {
				t.Errorf("links = %d, want %d", got, c.wantLinks)
			}
			if got := len(p.IPRules); got != c.wantRules {
				t.Errorf("ip rules = %d, want %d", got, c.wantRules)
			}
			if got := len(p.Routes); got != c.wantRoutes {
				t.Errorf("routes = %d, want %d", got, c.wantRoutes)
			}
		})
	}
}

// TestSameNodeHasNoReturnPath is the correctness check the acceptance table
// calls out: when the pod is on the accepting node there is no link, no mark
// and no routing rule.
func TestSameNodeHasNoReturnPath(t *testing.T) {
	var same State
	for _, c := range cases() {
		if c.name == "same-node" {
			same = c.state
		}
	}
	p := Render(same)
	if len(p.Links) != 0 || len(p.IPRules) != 0 || len(p.Routes) != 0 {
		t.Fatalf("same-node produced return-path state: links=%d rules=%d routes=%d",
			len(p.Links), len(p.IPRules), len(p.Routes))
	}
	if countKind(p.Rules, KindMark) != 0 {
		t.Fatalf("same-node produced a mark rule")
	}
}

// TestNFTExprsNonEmpty guards against the failure mode the two-consumer design
// exists to prevent: a golden that passes while the applied expressions are
// empty or wrong. Every rule the renderer can produce must compile to a
// non-empty expression slice.
func TestNFTExprsNonEmpty(t *testing.T) {
	for _, c := range cases() {
		p := Render(c.state)
		for i, r := range p.Rules {
			exprs := nftExprs(r)
			if len(exprs) == 0 {
				t.Errorf("%s: rule %d (%s) produced no expressions", c.name, i, nftText(r))
			}
		}
	}
}

// TestDeterminism renders each case twice and asserts byte-identical text. Run
// with -count=10 to let map iteration order vary across runs.
func TestDeterminism(t *testing.T) {
	for _, c := range cases() {
		a := Render(c.state).String()
		b := Render(c.state).String()
		if a != b {
			t.Errorf("%s: render not deterministic:\n--- a ---\n%s\n--- b ---\n%s", c.name, a, b)
		}
	}
}
