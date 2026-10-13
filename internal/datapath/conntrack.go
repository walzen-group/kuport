package datapath

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// DNATKey names a DNAT rule by what a mapping owns: the interface it accepts
// on, the protocol and dialed port range, and the target address.
type DNATKey struct {
	Iface, Proto string
	First, Last  uint16
	Target       netip.Addr
}

// KeyOf is the key of a DNAT rule.
func KeyOf(d DNATRule) DNATKey {
	return DNATKey{Iface: d.Iface, Proto: d.Port.Proto, First: d.Port.First, Last: d.Port.Last, Target: d.ToAddr.Unmap()}
}

// CountFlows counts the conntrack flows each DNAT rule made. A flow belongs to
// a rule when its protocol and original destination port fall in the rule's
// selection and its reply comes from the rule's target. A reply source that
// differs from the original destination is what a DNAT leaves in conntrack;
// netlink's ConntrackFlow carries no status to read it from.
//
// A pod's node under Multi can hold an accepting rule and a link rule for the
// same pod and port. The link rule pins the link end as destination, and
// conntrack records that address before kup-steer rewrites it, so a flow whose
// original destination is a rule's DstAddr belongs to that rule; any other
// flow belongs to the first matching rule with no DstAddr.
func CountFlows(flows []*netlink.ConntrackFlow, dnat []DNATRule) map[DNATKey]int {
	type cand struct {
		key DNATKey
		dst netip.Addr // the pinned destination, invalid when none
	}
	byTarget := map[netip.Addr][]cand{}
	for _, d := range dnat {
		c := cand{key: KeyOf(d)}
		if d.DstAddr != nil {
			c.dst = d.DstAddr.Unmap()
		}
		byTarget[c.key.Target] = append(byTarget[c.key.Target], c)
	}

	out := map[DNATKey]int{}
	for _, f := range flows {
		replySrc, ok1 := netip.AddrFromSlice(f.Reverse.SrcIP)
		origDst, ok2 := netip.AddrFromSlice(f.Forward.DstIP)
		if !ok1 || !ok2 {
			continue
		}
		replySrc, origDst = replySrc.Unmap(), origDst.Unmap()
		if replySrc == origDst {
			continue
		}
		cands := byTarget[replySrc]
		if len(cands) == 0 {
			continue
		}
		proto := protoName(f.Forward.Protocol)
		port := f.Forward.DstPort
		var pinned, open *DNATKey
		for i := range cands {
			c := &cands[i]
			if c.key.Proto != proto || port < c.key.First || port > c.key.Last {
				continue
			}
			switch {
			case c.dst.IsValid() && c.dst == origDst && pinned == nil:
				pinned = &c.key
			case !c.dst.IsValid() && open == nil:
				open = &c.key
			}
		}
		switch {
		case pinned != nil:
			out[*pinned]++
		case open != nil:
			out[*open]++
		}
	}
	return out
}

func protoName(p uint8) string {
	switch p {
	case unix.IPPROTO_TCP:
		return "tcp"
	case unix.IPPROTO_UDP:
		return "udp"
	}
	return ""
}

// dumpTimeout caps one conntrack dump; a scrape waits for it.
const dumpTimeout = 5 * time.Second

// ErrDumpTimeout is returned when a dump runs past dumpTimeout.
var ErrDumpTimeout = errors.New("conntrack dump ran past 5s")

// DumpFlows runs list and gives up after five seconds or when ctx ends. A dump
// that gives up keeps running in its goroutine until the kernel answers, and
// its result is dropped.
func DumpFlows(ctx context.Context, list func() ([]*netlink.ConntrackFlow, error)) ([]*netlink.ConntrackFlow, error) {
	ctx, cancel := context.WithTimeout(ctx, dumpTimeout)
	defer cancel()
	type result struct {
		flows []*netlink.ConntrackFlow
		err   error
	}
	done := make(chan result, 1)
	go func() {
		f, err := list()
		done <- result{f, err}
	}()
	select {
	case r := <-done:
		return r.flows, r.err
	case <-ctx.Done():
		return nil, ErrDumpTimeout
	}
}

// HostConntrack lists the node's conntrack table in both families.
func HostConntrack() ([]*netlink.ConntrackFlow, error) {
	var out []*netlink.ConntrackFlow
	for _, fam := range []netlink.InetFamily{unix.AF_INET, unix.AF_INET6} {
		f, err := netlink.ConntrackTableList(netlink.ConntrackTable, fam)
		if err != nil {
			return nil, fmt.Errorf("conntrack family %d: %w", fam, err)
		}
		out = append(out, f...)
	}
	return out, nil
}
