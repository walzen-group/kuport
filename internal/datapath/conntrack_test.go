package datapath

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func flow(proto uint8, origDst string, port uint16, replySrc string) *netlink.ConntrackFlow {
	return &netlink.ConntrackFlow{
		Forward: netlink.IPTuple{Protocol: proto, SrcIP: net.ParseIP("198.51.100.9"), DstIP: net.ParseIP(origDst), DstPort: port},
		Reverse: netlink.IPTuple{Protocol: proto, SrcIP: net.ParseIP(replySrc), DstIP: net.ParseIP("198.51.100.9")},
	}
}

// TestCountFlows counts a pod's node under Multi: an accepting rule on enp1s0
// and a link rule on kup-0 for the same pod and range.
func TestCountFlows(t *testing.T) {
	end := netip.MustParseAddr("169.254.64.1")
	udp := PortSel{"udp", 27015, 27115}
	dnat := []DNATRule{
		{Iface: "enp1s0", Port: udp, ToAddr: pod},
		{Iface: "kup-0", Port: udp, ToAddr: pod, DstAddr: &end},
	}
	flows := []*netlink.ConntrackFlow{
		flow(unix.IPPROTO_UDP, "203.0.113.5", 27020, pod.String()), // accepted on enp1s0
		flow(unix.IPPROTO_UDP, end.String(), 27020, pod.String()),  // delivered over kup-0
		flow(unix.IPPROTO_UDP, "203.0.113.5", 28000, pod.String()), // outside the range
		flow(unix.IPPROTO_UDP, pod.String(), 27020, pod.String()),  // dialed straight to the pod, no DNAT
		flow(unix.IPPROTO_TCP, "203.0.113.5", 27020, pod.String()), // another protocol
		flow(unix.IPPROTO_UDP, "203.0.113.5", 27020, "10.244.9.9"), // another target
	}
	got := CountFlows(flows, dnat)
	want := map[DNATKey]int{KeyOf(dnat[0]): 1, KeyOf(dnat[1]): 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("counts:\n got %v\nwant %v", got, want)
	}
}

// TestDumpFlowsGivesUp returns at the context's end with ErrDumpTimeout when
// the dump does not answer.
func TestDumpFlowsGivesUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	block := make(chan struct{})
	defer close(block)
	_, err := DumpFlows(ctx, func() ([]*netlink.ConntrackFlow, error) { <-block; return nil, nil })
	if !errors.Is(err, ErrDumpTimeout) {
		t.Errorf("err = %v, want ErrDumpTimeout", err)
	}
}

// BenchmarkCountFlows50k counts 50000 flows against 20 rules, the scale the
// spec records the scrape time at.
func BenchmarkCountFlows50k(b *testing.B) {
	var dnat []DNATRule
	for i := range 20 {
		a := netip.AddrFrom4([4]byte{10, 244, 1, byte(i + 1)})
		dnat = append(dnat, DNATRule{Iface: "wt0", Port: PortSel{"udp", uint16(20000 + i*10), uint16(20009 + i*10)}, ToAddr: a})
	}
	flows := make([]*netlink.ConntrackFlow, 0, 50000)
	for i := range 50000 {
		target := netip.AddrFrom4([4]byte{10, 244, byte(i % 3), byte(i%40 + 1)})
		flows = append(flows, flow(unix.IPPROTO_UDP, "100.64.1.1", uint16(20000+i%250), target.String()))
	}
	b.ResetTimer()
	for b.Loop() {
		CountFlows(flows, dnat)
	}
}
