package datapath

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
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
	if got := nftText(r); got != `tcp dport 10002 counter dnat to 10.244.5.5:8096` {
		t.Errorf("text = %q", got)
	}
}
