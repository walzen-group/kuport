package datapath

import (
	"bytes"
	"net/netip"
	"reflect"
	"testing"

	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// TestDNATExprsTranslatePort checks the applied expressions, the sibling the
// goldens do not read: a ToPort loads that port as the NAT's only proto value.
func TestDNATExprsTranslatePort(t *testing.T) {
	r := Rule{
		Chain:  ChainPre,
		Kind:   KindDNAT,
		Port:   PortSel{Proto: "tcp", First: 10002, Last: 10002},
		ToAddr: netip.MustParseAddr("10.244.5.5"),
		ToPort: 8096,
	}
	var nat *expr.NAT
	var protoMin []byte
	for _, e := range nftExprs(r) {
		switch v := e.(type) {
		case *expr.Immediate:
			if v.Register == 2 {
				protoMin = v.Data
			}
		case *expr.NAT:
			nat = v
		}
	}
	if nat == nil {
		t.Fatal("no NAT expression")
	}
	if want := binaryutil.BigEndian.PutUint16(8096); !bytes.Equal(protoMin, want) {
		t.Errorf("proto register = %v, want %v (8096)", protoMin, want)
	}
	if nat.RegProtoMax != nat.RegProtoMin {
		t.Errorf("NAT proto range %d-%d, want the single register %d", nat.RegProtoMin, nat.RegProtoMax, nat.RegProtoMin)
	}
	if got := nftText(r); got != `meta nfproto ipv4 tcp dport 10002 counter dnat ip to 10.244.5.5:8096` {
		t.Errorf("text = %q", got)
	}
}

// familyExprs is what the applied expressions of one rule say about its family:
// the nfproto value it matches first, every network-header load as offset and
// length, the width of the NAT target address, and the NAT family.
type familyExprs struct {
	nfproto  int // -1 when the rule carries no nfproto match
	loads    [][2]uint32
	natAddr  int
	natFamil uint32
}

func readFamilyExprs(t *testing.T, r Rule) familyExprs {
	t.Helper()
	got := familyExprs{nfproto: -1}
	imm1 := 0 // width of the last immediate into register 1, read by a NAT
	exprs := nftExprs(r)
	for i, e := range exprs {
		switch v := e.(type) {
		case *expr.Meta:
			if v.Key != expr.MetaKeyNFPROTO {
				continue
			}
			if i != 0 {
				t.Errorf("nfproto match at expression %d, want it first", i)
			}
			cmp, ok := exprs[i+1].(*expr.Cmp)
			if !ok || len(cmp.Data) != 1 {
				t.Fatalf("nfproto load not followed by a one-byte compare: %#v", exprs[i+1])
			}
			got.nfproto = int(cmp.Data[0])
		case *expr.Payload:
			if v.Base == expr.PayloadBaseNetworkHeader {
				got.loads = append(got.loads, [2]uint32{v.Offset, v.Len})
			}
		case *expr.Immediate:
			if v.Register == 1 {
				imm1 = len(v.Data)
			}
		case *expr.NAT:
			got.natFamil = v.Family
			if v.RegAddrMin == 1 {
				got.natAddr = imm1
			}
		}
	}
	return got
}

// TestNFTExprsFamily checks the applied expressions of every rule kind in both
// families. An inet table hands a rule packets of either family, so a rule that
// loads an address must first match the family, and must load at that family's
// offsets: an IPv4 load on an IPv6 packet reads the middle of its source
// address and matches nothing anyone wrote.
func TestNFTExprsFamily(t *testing.T) {
	v4 := netip.MustParseAddr("10.244.5.5")
	v6 := netip.MustParseAddr("fd00:10:244:5::5")
	link4 := netip.MustParseAddr("169.254.77.1")
	link6 := netip.MustParseAddr("fd64:f5ac:e961::1")
	udp := PortSel{Proto: "udp", First: 3000, Last: 3000}

	tests := []struct {
		name string
		rule Rule
		want familyExprs
	}{
		{"dnat ipv4", Rule{Kind: KindDNAT, IifName: "wt0", Port: udp, ToAddr: v4},
			familyExprs{nfproto: unix.NFPROTO_IPV4, natAddr: 4, natFamil: unix.NFPROTO_IPV4}},
		{"dnat ipv6", Rule{Kind: KindDNAT, IifName: "wt0", Port: udp, ToAddr: v6},
			familyExprs{nfproto: unix.NFPROTO_IPV6, natAddr: 16, natFamil: unix.NFPROTO_IPV6}},
		{"link dnat ipv4", Rule{Kind: KindDNAT, IifName: "kup-x", DstAddr: &link4, Port: udp, ToAddr: v4},
			familyExprs{nfproto: unix.NFPROTO_IPV4, loads: [][2]uint32{{16, 4}}, natAddr: 4, natFamil: unix.NFPROTO_IPV4}},
		{"link dnat ipv6", Rule{Kind: KindDNAT, IifName: "kup-x", DstAddr: &link6, Port: udp, ToAddr: v6},
			familyExprs{nfproto: unix.NFPROTO_IPV6, loads: [][2]uint32{{24, 16}}, natAddr: 16, natFamil: unix.NFPROTO_IPV6}},
		{"exempt ipv4", Rule{Kind: KindSNAT, OifName: "cilium_host", DstAddr: &v4, Port: udp},
			familyExprs{nfproto: unix.NFPROTO_IPV4, loads: [][2]uint32{{16, 4}, {12, 4}}, natFamil: unix.NFPROTO_IPV4}},
		{"exempt ipv6", Rule{Kind: KindSNAT, OifName: "cilium_host", DstAddr: &v6, Port: udp},
			familyExprs{nfproto: unix.NFPROTO_IPV6, loads: [][2]uint32{{24, 16}, {8, 16}}, natFamil: unix.NFPROTO_IPV6}},
		{"reply exempt ipv6", Rule{Kind: KindSNAT, OifName: "cilium_*", OifNeg: true, SrcAddr: &v6, Port: udp, PortIsSrc: true},
			familyExprs{nfproto: unix.NFPROTO_IPV6, loads: [][2]uint32{{8, 16}, {8, 16}}, natFamil: unix.NFPROTO_IPV6}},
		{"mark ipv4", Rule{Kind: KindMark, SrcAddr: &v4, Port: udp, PortIsSrc: true, Mark: 0x6b700001},
			familyExprs{nfproto: unix.NFPROTO_IPV4, loads: [][2]uint32{{12, 4}}}},
		{"mark ipv6", Rule{Kind: KindMark, SrcAddr: &v6, Port: udp, PortIsSrc: true, Mark: 0x6b700001},
			familyExprs{nfproto: unix.NFPROTO_IPV6, loads: [][2]uint32{{8, 16}}}},
		{"ct load ipv6", Rule{Kind: KindCtLoad, SrcAddr: &v6, Port: udp, PortIsSrc: true},
			familyExprs{nfproto: unix.NFPROTO_IPV6, loads: [][2]uint32{{8, 16}}}},
		{"ct save", Rule{Kind: KindCtSave, IifName: "kup-x", Mark: 0x6b700001},
			familyExprs{nfproto: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readFamilyExprs(t, tt.rule)
			if got.nfproto != tt.want.nfproto {
				t.Errorf("nfproto = %d, want %d", got.nfproto, tt.want.nfproto)
			}
			if !reflect.DeepEqual(got.loads, tt.want.loads) {
				t.Errorf("network-header loads (offset, len) = %v, want %v", got.loads, tt.want.loads)
			}
			if got.natAddr != tt.want.natAddr {
				t.Errorf("NAT address width = %d bytes, want %d", got.natAddr, tt.want.natAddr)
			}
			if got.natFamil != tt.want.natFamil {
				t.Errorf("NAT family = %d, want %d", got.natFamil, tt.want.natFamil)
			}
		})
	}
}
