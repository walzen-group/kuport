package datapath

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// CounterValue is what one nftables counter holds.
type CounterValue struct{ Packets, Bytes uint64 }

// LiveKey names the series a live rule feeds: every field the metrics label it
// by. Two rules with the same key count the same packets.
type LiveKey struct {
	Chain    string
	Iif, Oif string
	Family   Family
	Proto    string // "tcp", "udp", "" for none
	Port     PortSel
	Dir      CtDirection
	Target   netip.Addr // kup-pre: DNAT target; kup-count: ip daddr (request) or ip saddr (reply)
	Src      netip.Addr // kup-egress: ip saddr
	Mark     uint32     // kup-egress: mark set
}

// LiveRule is one rule read back from table inet kuport with its counter.
type LiveRule struct {
	LiveKey
	Counter CounterValue
}

// Key is every field but the counter.
func (l LiveRule) Key() LiveKey { return l.LiveKey }

// CounterReader lists a chain's rules with their expressions and counters.
// *nftables.Conn implements it.
type CounterReader interface {
	GetRules(*nftables.Table, *nftables.Chain) ([]*nftables.Rule, error)
}

// liveChains are the chains whose counters feed a metric.
var liveChains = []string{ChainPre, ChainCount, ChainEgress, ChainMSS}

// ReadLive lists the counted chains of table inet kuport and parses each rule.
func ReadLive(r CounterReader) ([]LiveRule, error) {
	if r == nil {
		return nil, errors.New("no nftables connection to read counters from")
	}
	t := nftTable()
	var out []LiveRule
	for _, name := range liveChains {
		rules, err := r.GetRules(t, &nftables.Chain{Name: name, Table: t})
		if err != nil {
			return nil, fmt.Errorf("rules of %s: %w", name, err)
		}
		for _, rule := range rules {
			out = append(out, parseLiveRule(name, rule.Exprs))
		}
	}
	return out, nil
}

// parseLiveRule reads a rule's expressions back into its key. It walks them in
// order, remembering what the last load put in register 1, and reads the value
// the following comparison holds. nftExprs writes every match this way, and
// TestParseLiveRule pins the two together.
func parseLiveRule(chain string, exprs []expr.Any) LiveRule {
	l := LiveRule{LiveKey: LiveKey{Chain: chain}}
	var src, dst netip.Addr
	var load expr.Any
	var imm []byte
	for _, e := range exprs {
		switch v := e.(type) {
		case *expr.Meta:
			if v.SourceRegister {
				// meta mark set: the mark is the immediate before it.
				if v.Key == expr.MetaKeyMARK && len(imm) == 4 {
					l.Mark = binary.NativeEndian.Uint32(imm)
				}
				load = nil
				continue
			}
			load = v
		case *expr.Payload, *expr.Ct:
			load = v
		case *expr.Immediate:
			if v.Register == 1 {
				imm = v.Data
			}
		case *expr.NAT:
			if v.Type == expr.NATTypeDestNAT {
				l.Target, _ = netip.AddrFromSlice(imm)
			}
		case *expr.Counter:
			l.Counter = CounterValue{Packets: v.Packets, Bytes: v.Bytes}
		case *expr.Cmp:
			if v.Op != expr.CmpOpEq {
				load = nil
				continue
			}
			readCmp(&l, load, v.Data, v.Data, &src, &dst)
			load = nil
		case *expr.Range:
			readCmp(&l, load, v.FromData, v.ToData, &src, &dst)
			load = nil
		default:
			load = nil
		}
	}
	switch chain {
	case ChainCount:
		if l.Dir == CtDirReply {
			l.Target = src
		} else {
			l.Target = dst
		}
	case ChainEgress:
		l.Src = src
	}
	return l
}

// readCmp stores the value a comparison holds against what load put in the
// register. A range holds two values; only a port is ever a range.
func readCmp(l *LiveRule, load expr.Any, from, to []byte, src, dst *netip.Addr) {
	switch v := load.(type) {
	case *expr.Meta:
		switch v.Key {
		case expr.MetaKeyNFPROTO:
			if len(from) == 1 && from[0] == unix.NFPROTO_IPV6 {
				l.Family = FamilyIPv6
			} else {
				l.Family = FamilyIPv4
			}
		case expr.MetaKeyIIFNAME:
			l.Iif = ifname(from)
		case expr.MetaKeyOIFNAME:
			l.Oif = ifname(from)
		case expr.MetaKeyL4PROTO:
			if len(from) == 1 && from[0] == unix.IPPROTO_TCP {
				l.Proto = "tcp"
			} else {
				l.Proto = "udp"
			}
			l.Port.Proto = l.Proto
		}
	case *expr.Payload:
		switch v.Base {
		case expr.PayloadBaseNetworkHeader:
			a, ok := netip.AddrFromSlice(from)
			if !ok {
				return
			}
			switch v.Offset {
			case ipSrcOffset, ip6SrcOffset:
				*src = a
			case ipDstOffset, ip6DstOffset:
				*dst = a
			}
			if a.Is6() {
				l.Family = FamilyIPv6
			}
		case expr.PayloadBaseTransportHeader:
			if len(from) != 2 || len(to) != 2 {
				return
			}
			l.Port.First = binary.BigEndian.Uint16(from)
			l.Port.Last = binary.BigEndian.Uint16(to)
		}
	case *expr.Ct:
		if v.Key == expr.CtKeyDIRECTION && len(from) == 1 {
			if from[0] == ipCtDirReply {
				l.Dir = CtDirReply
			} else {
				l.Dir = CtDirOriginal
			}
		}
	}
}

// ifname is an interface name as a comparison holds it: an exact name ends in
// a null byte, a prefix match does not and gets its star back.
func ifname(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b) + "*"
}

// Totals keeps each series' packets and bytes across applies. Apply replaces
// table inet kuport and every counter in it restarts at zero, so before each
// Apply the agent folds the live counters into the totals; a scrape adds what
// the live table has counted since.
type Totals struct {
	mu   sync.Mutex
	held map[LiveKey]CounterValue
}

// Fold adds the live counters to the totals. Call it right before an Apply.
func (t *Totals) Fold(live []LiveRule) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held == nil {
		t.held = map[LiveKey]CounterValue{}
	}
	for k, v := range distinct(live) {
		h := t.held[k]
		t.held[k] = CounterValue{Packets: h.Packets + v.Packets, Bytes: h.Bytes + v.Bytes}
	}
}

// Snapshot returns the totals plus the live counters, for every key either
// holds.
func (t *Totals) Snapshot(live []LiveRule) map[LiveKey]CounterValue {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[LiveKey]CounterValue, len(t.held))
	for k, v := range t.held {
		out[k] = v
	}
	for k, v := range distinct(live) {
		h := out[k]
		out[k] = CounterValue{Packets: h.Packets + v.Packets, Bytes: h.Bytes + v.Bytes}
	}
	return out
}

// Counters keeps a node's totals and reads them together with the live table.
// One lock covers the fold, the Apply after it and every read, so a read never
// sees the old table after its counters were folded.
type Counters struct {
	mu     sync.Mutex
	reader CounterReader
	totals Totals
}

// NewCounters reads live counters through r, the agent's nftables connection.
func NewCounters(r CounterReader) *Counters { return &Counters{reader: r} }

// FoldAndApply folds the live counters into the totals and runs apply. A failed
// read skips the fold: the table's counts since the last Apply are lost, and
// the Apply still runs.
func (c *Counters) FoldAndApply(apply func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live, err := ReadLive(c.reader); err == nil {
		c.totals.Fold(live)
	}
	return apply()
}

// Read returns every series' totals plus what the live table counted since the
// last Apply.
func (c *Counters) Read() (map[LiveKey]CounterValue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	live, err := ReadLive(c.reader)
	if err != nil {
		return nil, err
	}
	return c.totals.Snapshot(live), nil
}

// distinct folds rules sharing a key into one value. Two PortMaps that map
// different ports to the same target render identical kup-count rules, and
// both count the same packets, so the larger of the two is the traffic.
func distinct(live []LiveRule) map[LiveKey]CounterValue {
	out := make(map[LiveKey]CounterValue, len(live))
	for _, l := range live {
		k := l.Key()
		h := out[k]
		if l.Counter.Packets > h.Packets {
			h.Packets = l.Counter.Packets
		}
		if l.Counter.Bytes > h.Bytes {
			h.Bytes = l.Counter.Bytes
		}
		out[k] = h
	}
	return out
}
