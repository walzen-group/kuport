package reconcile

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/walzen-group/kuport/internal/datapath"
)

// TestMetricsIndexOwnsEveryDNAT checks both ends of a remote mapping: every
// DNAT entry in the State has its PortMap as owner, and the mapping's row
// describes it.
func TestMetricsIndexOwnsEveryDNAT(t *testing.T) {
	for _, node := range []string{"node-a", "node-b"} {
		t.Run(node, func(t *testing.T) {
			res := Compute(remoteWorld(node))
			if len(res.State.DNAT) == 0 {
				t.Fatal("no DNAT entries to own")
			}
			for _, d := range res.State.DNAT {
				if got := res.Metrics.DNAT[datapath.KeyOf(d)]; got != (Owner{Namespace: "games", Name: "a"}) {
					t.Errorf("owner of %+v = %+v, want games/a", d, got)
				}
			}
			want := []MappingInfo{{Namespace: "games", Name: "a", Class: "public", Mode: "Single", Protocol: "tcp", Port: "3000", EndpointNode: "node-b"}}
			if !reflect.DeepEqual(res.Metrics.Mappings, want) {
				t.Errorf("mappings:\n got %+v\nwant %+v", res.Metrics.Mappings, want)
			}
		})
	}
}

// TestMetricsIndexBystanderHoldsNothing: node-c holds no rule for the mapping
// and reports no row for it.
func TestMetricsIndexBystanderHoldsNothing(t *testing.T) {
	res := Compute(remoteWorld("node-c"))
	if len(res.Metrics.DNAT) != 0 || len(res.Metrics.Mappings) != 0 {
		t.Errorf("node-c index = %+v, want empty", res.Metrics)
	}
}

// TestMetricsIndexOwnsEgress checks a linked Egress pod and a blocked one on the
// pod's node.
func TestMetricsIndexOwnsEgress(t *testing.T) {
	pod := netip.MustParseAddr("10.244.5.5")
	tests := []struct {
		name string
		in   Inputs
		want EgressOwner
	}{
		{"link", egressWorld("node-b", true), EgressOwner{Namespace: "media", Egress: "qb", Pod: "qb-0"}},
		{"blocked", func() Inputs { in := egressWorld("node-b", true); in.Links = nil; return in }(), EgressOwner{Namespace: "media", Egress: "qb", Pod: "qb-0", Blocked: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compute(tt.in)
			if len(res.State.Egress) == 0 {
				t.Fatal("no egress entries to own")
			}
			for _, e := range res.State.Egress {
				if got := res.Metrics.Egress[e.SrcAddr]; got != tt.want {
					t.Errorf("owner of %s = %+v, want %+v", e.SrcAddr, got, tt.want)
				}
			}
			if _, ok := res.Metrics.Egress[pod]; !ok {
				t.Errorf("no owner for the pod's address %s", pod)
			}
		})
	}
}
