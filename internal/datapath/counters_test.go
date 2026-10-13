package datapath

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/google/nftables/expr"
)

// withCounter returns a rule's expressions with its counter set, the way the
// kernel lists a rule that has counted.
func withCounter(exprs []expr.Any, c CounterValue) []expr.Any {
	out := make([]expr.Any, len(exprs))
	for i, e := range exprs {
		if _, ok := e.(*expr.Counter); ok {
			e = &expr.Counter{Packets: c.Packets, Bytes: c.Bytes}
		}
		out[i] = e
	}
	return out
}

// TestParseLiveRule builds each counted rule kind with nftExprs and parses it
// back, so parse and render cannot drift apart.
func TestParseLiveRule(t *testing.T) {
	link6 := netip.MustParseAddr("fd64:f5ac:e961::1")
	src := netip.MustParseAddr("10.244.3.7")
	cnt := CounterValue{Packets: 3, Bytes: 30}
	tests := []struct {
		name string
		rule Rule
		want LiveKey
	}{
		{
			name: "dnat range",
			rule: Rule{Chain: ChainPre, Kind: KindDNAT, IifName: "wt0", Port: PortSel{"udp", 27015, 27115}, ToAddr: pod},
			want: LiveKey{Chain: ChainPre, Iif: "wt0", Family: FamilyIPv4, Proto: "udp", Port: PortSel{"udp", 27015, 27115}, Target: pod},
		},
		{
			name: "link dnat translating ipv6",
			rule: Rule{Chain: ChainPre, Kind: KindDNAT, IifName: "kup-3", DstAddr: &link6, Port: PortSel{"tcp", 10002, 10002}, ToAddr: pod6, ToPort: 8096},
			want: LiveKey{Chain: ChainPre, Iif: "kup-3", Family: FamilyIPv6, Proto: "tcp", Port: PortSel{"tcp", 10002, 10002}, Target: pod6},
		},
		{
			name: "count request",
			rule: Rule{Chain: ChainCount, Kind: KindCount, IifName: "enp1s0", DstAddr: &pod, Port: PortSel{"tcp", 8096, 8096}, CtDir: CtDirOriginal},
			want: LiveKey{Chain: ChainCount, Iif: "enp1s0", Family: FamilyIPv4, Proto: "tcp", Port: PortSel{"tcp", 8096, 8096}, Dir: CtDirOriginal, Target: pod},
		},
		{
			name: "count reply range ipv6",
			rule: Rule{Chain: ChainCount, Kind: KindCount, OifName: "wt0", SrcAddr: &pod6, Port: PortSel{"udp", 27015, 27115}, PortIsSrc: true, CtDir: CtDirReply},
			want: LiveKey{Chain: ChainCount, Oif: "wt0", Family: FamilyIPv6, Proto: "udp", Port: PortSel{"udp", 27015, 27115}, Dir: CtDirReply, Target: pod6},
		},
		{
			name: "egress link mark",
			rule: Rule{Chain: ChainEgress, Kind: KindMark, SrcAddr: &src, DstOutsideCluster: true, Mark: 0x6b700002},
			want: LiveKey{Chain: ChainEgress, Family: FamilyIPv4, Src: src, Mark: 0x6b700002},
		},
		{
			name: "egress blocked",
			rule: Rule{Chain: ChainEgress, Kind: KindMark, SrcAddr: &src, DstOutsideCluster: true, Mark: EgressBlockedMark},
			want: LiveKey{Chain: ChainEgress, Family: FamilyIPv4, Src: src, Mark: EgressBlockedMark},
		},
		{
			name: "mss",
			rule: Rule{Chain: ChainMSS, Kind: KindMSS, OifName: "kup-*"},
			want: LiveKey{Chain: ChainMSS, Oif: "kup-*", Proto: "tcp", Port: PortSel{Proto: "tcp"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLiveRule(tt.rule.Chain, withCounter(nftExprs(tt.rule, nil), cnt))
			if got.Key() != tt.want {
				t.Errorf("key:\n got %+v\nwant %+v", got.Key(), tt.want)
			}
			if got.Counter != cnt {
				t.Errorf("counter = %+v, want %+v", got.Counter, cnt)
			}
		})
	}
}

// TestTotalsSurviveApplies folds two applies' worth of counters and reads a
// snapshot with the live table on top.
func TestTotalsSurviveApplies(t *testing.T) {
	a := LiveKey{Chain: ChainCount, Iif: "wt0", Proto: "udp", Target: pod}
	b := LiveKey{Chain: ChainCount, Oif: "wt0", Proto: "udp", Target: pod}
	c := LiveKey{Chain: ChainPre, Iif: "wt0", Proto: "udp", Target: pod}

	var tot Totals
	tot.Fold([]LiveRule{{a, CounterValue{3, 300}}, {b, CounterValue{4, 400}}})
	tot.Fold([]LiveRule{{a, CounterValue{2, 200}}})
	got := tot.Snapshot([]LiveRule{{a, CounterValue{1, 100}}, {c, CounterValue{7, 700}}})
	want := map[LiveKey]CounterValue{
		a: {6, 600}, // 3 + 2 folded, 1 live
		b: {4, 400}, // only in the totals: its rule went away
		c: {7, 700}, // only live: a rule the last apply added
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot:\n got %v\nwant %v", got, want)
	}
}

// TestTotalsCountSharedRuleOnce reads two identical kup-count rules, the pair
// two PortMaps on one target port render, as one series.
func TestTotalsCountSharedRuleOnce(t *testing.T) {
	a := LiveKey{Chain: ChainCount, Iif: "wt0", Proto: "tcp", Port: PortSel{"tcp", 8096, 8096}, Target: pod}
	var tot Totals
	tot.Fold([]LiveRule{{a, CounterValue{5, 500}}, {a, CounterValue{5, 500}}})
	if got := tot.Snapshot(nil)[a]; got != (CounterValue{5, 500}) {
		t.Errorf("shared rule = %+v, want 5 packets, 500 bytes", got)
	}
}
