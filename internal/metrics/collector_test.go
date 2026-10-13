package metrics

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/walzen-group/kuport/internal/datapath"
	"github.com/walzen-group/kuport/internal/reconcile"
)

type fakeSource struct {
	snap     Snapshot
	counters map[datapath.LiveKey]datapath.CounterValue
	flows    []*netlink.ConntrackFlow
	flowErr  error
}

func (f fakeSource) Snapshot() Snapshot { return f.snap }
func (f fakeSource) Counters() (map[datapath.LiveKey]datapath.CounterValue, error) {
	return f.counters, nil
}
func (f fakeSource) Flows(context.Context) ([]*netlink.ConntrackFlow, error) {
	return f.flows, f.flowErr
}

var (
	jfPod   = netip.MustParseAddr("10.244.1.5")
	tsPod   = netip.MustParseAddr("10.244.2.6")
	linkEnd = netip.MustParseAddr("169.254.64.1")
	qb0     = netip.MustParseAddr("10.244.3.3")
	qb1     = netip.MustParseAddr("10.244.3.4")
)

// world is a node that accepts jellyfin on wt0 (10002 translated to 8096) and
// delivers teamspeak over kup-0 for a remote accepting node.
func world() fakeSource {
	jf := datapath.DNATRule{Iface: "wt0", Port: datapath.PortSel{Proto: "tcp", First: 10002, Last: 10002}, ToAddr: jfPod, ToPort: 8096}
	ts := datapath.DNATRule{Iface: "kup-0", Port: datapath.PortSel{Proto: "udp", First: 9987, Last: 9987}, ToAddr: tsPod, DstAddr: &linkEnd}
	tcp8096 := datapath.PortSel{Proto: "tcp", First: 8096, Last: 8096}
	udp9987 := datapath.PortSel{Proto: "udp", First: 9987, Last: 9987}
	jfRequest := datapath.LiveKey{Chain: datapath.ChainCount, Iif: "wt0", Proto: "tcp", Port: tcp8096, Dir: datapath.CtDirOriginal, Target: jfPod}
	jfReply := datapath.LiveKey{Chain: datapath.ChainCount, Oif: "wt0", Proto: "tcp", Port: tcp8096, Dir: datapath.CtDirReply, Target: jfPod}
	tsDeliver := datapath.LiveKey{Chain: datapath.ChainCount, Iif: "kup-0", Proto: "udp", Port: udp9987, Dir: datapath.CtDirOriginal, Target: tsPod}
	jfNew := datapath.LiveKey{Chain: datapath.ChainPre, Iif: "wt0", Proto: "tcp", Port: jf.Port, Target: jfPod}
	qb0Link := datapath.LiveKey{Chain: datapath.ChainEgress, Src: qb0, Mark: 0x6b700003}
	qb1Blocked := datapath.LiveKey{Chain: datapath.ChainEgress, Src: qb1, Mark: datapath.EgressBlockedMark}
	mss := datapath.LiveKey{Chain: datapath.ChainMSS, Oif: "kup-*", Proto: "tcp"}
	// netbird inserts accept rules for its interface into every filter chain
	// on the forward hook, kup-mss included:
	//   oifname "wt0" ct state established,related counter accept
	//   iifname "wt0" counter accept
	nbOut := datapath.LiveKey{Chain: datapath.ChainMSS, Oif: "wt0"}
	nbIn := datapath.LiveKey{Chain: datapath.ChainMSS, Iif: "wt0"}
	return fakeSource{
		snap: Snapshot{
			Index: reconcile.MetricsIndex{
				DNAT: map[datapath.DNATKey]reconcile.Owner{
					datapath.KeyOf(jf): {Namespace: "media", Name: "jellyfin"},
					datapath.KeyOf(ts): {Namespace: "games", Name: "teamspeak"},
				},
				Egress: map[netip.Addr]reconcile.EgressOwner{
					qb0: {Namespace: "media", Egress: "qb", Pod: "qb-0"},
					qb1: {Namespace: "media", Egress: "qb", Pod: "qb-1", Blocked: true},
				},
				Mappings: []reconcile.MappingInfo{
					{Namespace: "games", Name: "teamspeak", Class: "public", Mode: "Single", Protocol: "udp", Port: "9987", EndpointNode: "node-b"},
					{Namespace: "media", Name: "jellyfin", Class: "overlay", Mode: "Multi", Protocol: "tcp", Port: "10002", EndpointNode: "node-a"},
				},
			},
			DNAT:  []datapath.DNATRule{jf, ts},
			Links: []LinkInfo{{Device: "kup-0", Slot: "0", Class: "public", Peer: "node-c"}},
		},
		counters: map[datapath.LiveKey]datapath.CounterValue{
			jfRequest:  {Packets: 10, Bytes: 1000},
			jfReply:    {Packets: 8, Bytes: 4000},
			tsDeliver:  {Packets: 5, Bytes: 500},
			jfNew:      {Packets: 2, Bytes: 120},
			qb0Link:    {Packets: 4, Bytes: 400},
			qb1Blocked: {Packets: 1, Bytes: 60},
			mss:        {Packets: 7, Bytes: 420},
			nbOut:      {Packets: 2, Bytes: 104},
			nbIn:       {Packets: 3, Bytes: 830},
		},
		flows: []*netlink.ConntrackFlow{
			ctFlow(unix.IPPROTO_TCP, "100.64.0.1", 10002, jfPod.String()),
			ctFlow(unix.IPPROTO_UDP, linkEnd.String(), 9987, tsPod.String()),
		},
	}
}

func ctFlow(proto uint8, origDst string, port uint16, replySrc string) *netlink.ConntrackFlow {
	return &netlink.ConntrackFlow{
		Forward: netlink.IPTuple{Protocol: proto, SrcIP: net.ParseIP("198.51.100.9"), DstIP: net.ParseIP(origDst), DstPort: port},
		Reverse: netlink.IPTuple{Protocol: proto, SrcIP: net.ParseIP(replySrc), DstIP: net.ParseIP("198.51.100.9")},
	}
}

func TestCollect(t *testing.T) {
	want := `
# HELP kuport_egress_bytes_total Bytes kup-egress marked, by path (link, blocked).
# TYPE kuport_egress_bytes_total counter
kuport_egress_bytes_total{egress="qb",family="ipv4",namespace="media",path="blocked",pod="qb-1"} 60
kuport_egress_bytes_total{egress="qb",family="ipv4",namespace="media",path="link",pod="qb-0"} 400
# HELP kuport_egress_packets_total Packets kup-egress marked, by path (link, blocked).
# TYPE kuport_egress_packets_total counter
kuport_egress_packets_total{egress="qb",family="ipv4",namespace="media",path="blocked",pod="qb-1"} 1
kuport_egress_packets_total{egress="qb",family="ipv4",namespace="media",path="link",pod="qb-0"} 4
# HELP kuport_link_info A return-link device this node holds.
# TYPE kuport_link_info gauge
kuport_link_info{class="public",device="kup-0",peer="node-c",slot="0"} 1
# HELP kuport_mapping_bytes_total Bytes of a mapping's flows, counted by kup-count as they leave the node.
# TYPE kuport_mapping_bytes_total counter
kuport_mapping_bytes_total{direction="reply",family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 4000
kuport_mapping_bytes_total{direction="request",family="ipv4",interface="kup-0",namespace="games",portmap="teamspeak",role="deliver"} 500
kuport_mapping_bytes_total{direction="request",family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 1000
# HELP kuport_mapping_connections_total Flows kup-pre translated; it sees only a flow's first packet.
# TYPE kuport_mapping_connections_total counter
kuport_mapping_connections_total{family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 2
# HELP kuport_mapping_info A mapping this node holds a DNAT rule for.
# TYPE kuport_mapping_info gauge
kuport_mapping_info{class="overlay",endpoint_node="node-a",mode="Multi",namespace="media",port="10002",portmap="jellyfin",protocol="tcp"} 1
kuport_mapping_info{class="public",endpoint_node="node-b",mode="Single",namespace="games",port="9987",portmap="teamspeak",protocol="udp"} 1
# HELP kuport_mapping_open_flows Conntrack entries a mapping's DNAT made.
# TYPE kuport_mapping_open_flows gauge
kuport_mapping_open_flows{namespace="games",portmap="teamspeak",role="deliver"} 1
kuport_mapping_open_flows{namespace="media",portmap="jellyfin",role="accept"} 1
# HELP kuport_mapping_packets_total Packets of a mapping's flows, counted by kup-count as they leave the node.
# TYPE kuport_mapping_packets_total counter
kuport_mapping_packets_total{direction="reply",family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 8
kuport_mapping_packets_total{direction="request",family="ipv4",interface="kup-0",namespace="games",portmap="teamspeak",role="deliver"} 5
kuport_mapping_packets_total{direction="request",family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 10
# HELP kuport_mss_clamped_total TCP SYNs kup-mss clamped on their way into a link.
# TYPE kuport_mss_clamped_total counter
kuport_mss_clamped_total 7
`
	if err := testutil.CollectAndCompare(New(world()), strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}

// TestCollectSharedCountRule gives two PortMaps that map 10002 and 10003 to the
// same pod port one shared kup-count rule; each PortMap reports its traffic.
func TestCollectSharedCountRule(t *testing.T) {
	a := datapath.DNATRule{Iface: "wt0", Port: datapath.PortSel{Proto: "tcp", First: 10002, Last: 10002}, ToAddr: jfPod, ToPort: 8096}
	b := datapath.DNATRule{Iface: "wt0", Port: datapath.PortSel{Proto: "tcp", First: 10003, Last: 10003}, ToAddr: jfPod, ToPort: 8096}
	src := fakeSource{
		snap: Snapshot{
			Index: reconcile.MetricsIndex{DNAT: map[datapath.DNATKey]reconcile.Owner{
				datapath.KeyOf(a): {Namespace: "media", Name: "a"},
				datapath.KeyOf(b): {Namespace: "media", Name: "b"},
			}},
			DNAT: []datapath.DNATRule{a, b},
		},
		counters: map[datapath.LiveKey]datapath.CounterValue{
			{Chain: datapath.ChainCount, Iif: "wt0", Proto: "tcp", Port: datapath.PortSel{Proto: "tcp", First: 8096, Last: 8096}, Dir: datapath.CtDirOriginal, Target: jfPod}: {Packets: 6, Bytes: 600},
		},
	}
	want := `
# HELP kuport_mapping_packets_total Packets of a mapping's flows, counted by kup-count as they leave the node.
# TYPE kuport_mapping_packets_total counter
kuport_mapping_packets_total{direction="request",family="ipv4",interface="wt0",namespace="media",portmap="a",role="accept"} 6
kuport_mapping_packets_total{direction="request",family="ipv4",interface="wt0",namespace="media",portmap="b",role="accept"} 6
`
	if err := testutil.CollectAndCompare(New(src), strings.NewReader(want), "kuport_mapping_packets_total"); err != nil {
		t.Error(err)
	}
}

// TestCollectDumpError leaves open flows out of the scrape and counts the error.
func TestCollectDumpError(t *testing.T) {
	src := world()
	src.flowErr = errors.New("dump interrupted")
	before := testutil.ToFloat64(ConntrackDumpErrors)
	if n := testutil.CollectAndCount(New(src), "kuport_mapping_open_flows"); n != 0 {
		t.Errorf("open flow series = %d, want 0 after a failed dump", n)
	}
	if got := testutil.ToFloat64(ConntrackDumpErrors) - before; got != 1 {
		t.Errorf("dump errors rose by %v, want 1", got)
	}
}
