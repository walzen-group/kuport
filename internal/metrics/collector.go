// Package metrics turns what a kuport agent programmed on its node into
// Prometheus series: per-mapping traffic from kup-count, new flows from
// kup-pre, open flows from conntrack, egress and MSS counters, the node's
// return links, and the agent's own applies.
package metrics

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink"

	"github.com/walzen-group/kuport/internal/datapath"
	"github.com/walzen-group/kuport/internal/reconcile"
)

// LinkInfo is one return-link device this node holds.
type LinkInfo struct{ Device, Slot, Class, Peer string }

// Snapshot is what the last successful apply programmed: the owner index, the
// DNAT entries and the links.
type Snapshot struct {
	Index reconcile.MetricsIndex
	DNAT  []datapath.DNATRule
	Links []LinkInfo
}

// Source is the agent side the collector reads on each scrape.
type Source interface {
	Snapshot() Snapshot
	Counters() (map[datapath.LiveKey]datapath.CounterValue, error)
	Flows(ctx context.Context) ([]*netlink.ConntrackFlow, error)
}

// The agent's own series, which applyState records.
var (
	ApplyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kuport_apply_total",
		Help: "Datapath applies, by result (ok, error).",
	}, []string{"result"})
	LastApplySuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kuport_last_apply_success_timestamp_seconds",
		Help: "When the datapath was last applied without an error.",
	})
	StateEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kuport_state_entries",
		Help: "Entries of the last applied state, by kind (dnat, exempt, mark, link, rule, route).",
	}, []string{"kind"})
	ConntrackDumpErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kuport_conntrack_dump_errors_total",
		Help: "Conntrack dumps that failed or ran past 5 seconds; that scrape has no open flows.",
	})
)

var (
	descMappingInfo = prometheus.NewDesc("kuport_mapping_info",
		"A mapping this node holds a DNAT rule for.",
		[]string{"namespace", "portmap", "class", "mode", "protocol", "port", "endpoint_node"}, nil)
	descPackets = prometheus.NewDesc("kuport_mapping_packets_total",
		"Packets of a mapping's flows, counted by kup-count as they leave the node.",
		[]string{"namespace", "portmap", "interface", "family", "role", "direction"}, nil)
	descBytes = prometheus.NewDesc("kuport_mapping_bytes_total",
		"Bytes of a mapping's flows, counted by kup-count as they leave the node.",
		[]string{"namespace", "portmap", "interface", "family", "role", "direction"}, nil)
	descConnections = prometheus.NewDesc("kuport_mapping_connections_total",
		"Flows kup-pre translated; it sees only a flow's first packet.",
		[]string{"namespace", "portmap", "interface", "family", "role"}, nil)
	descOpenFlows = prometheus.NewDesc("kuport_mapping_open_flows",
		"Conntrack entries a mapping's DNAT made.",
		[]string{"namespace", "portmap", "role"}, nil)
	descEgressPackets = prometheus.NewDesc("kuport_egress_packets_total",
		"Packets kup-egress marked, by path (link, blocked).",
		[]string{"namespace", "egress", "pod", "family", "path"}, nil)
	descEgressBytes = prometheus.NewDesc("kuport_egress_bytes_total",
		"Bytes kup-egress marked, by path (link, blocked).",
		[]string{"namespace", "egress", "pod", "family", "path"}, nil)
	descMSS = prometheus.NewDesc("kuport_mss_clamped_total",
		"TCP SYNs kup-mss clamped on their way into a link.", nil, nil)
	descLinkInfo = prometheus.NewDesc("kuport_link_info",
		"A return-link device this node holds.",
		[]string{"device", "slot", "class", "peer"}, nil)
)

// Collector reads a Source on every scrape.
type Collector struct{ src Source }

// New returns a Collector over src.
func New(src Source) *Collector { return &Collector{src: src} }

// Register adds the collector and the agent's own series to reg.
func Register(reg prometheus.Registerer, c *Collector) error {
	for _, m := range []prometheus.Collector{c, ApplyTotal, LastApplySuccess, StateEntries, ConntrackDumpErrors} {
		if err := reg.Register(m); err != nil {
			return err
		}
	}
	return nil
}

// Describe sends every series the collector can produce.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descMappingInfo, descPackets, descBytes, descConnections, descOpenFlows, descEgressPackets, descEgressBytes, descMSS, descLinkInfo} {
		ch <- d
	}
}

// Collect reads the Source once and sends every series.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	snap := c.src.Snapshot()
	for _, m := range snap.Index.Mappings {
		ch <- prometheus.MustNewConstMetric(descMappingInfo, prometheus.GaugeValue, 1,
			m.Namespace, m.Name, m.Class, m.Mode, m.Protocol, m.Port, m.EndpointNode)
	}
	for _, l := range snap.Links {
		ch <- prometheus.MustNewConstMetric(descLinkInfo, prometheus.GaugeValue, 1, l.Device, l.Slot, l.Class, l.Peer)
	}
	if counters, err := c.src.Counters(); err == nil {
		collectCounters(ch, snap, counters)
	}
	collectOpenFlows(ch, c.src, snap)
}

// series is one label set and the value summed into it.
type series struct {
	desc   *prometheus.Desc
	labels [8]string
	n      int
}

func (s series) values() []string { return s.labels[:s.n] }

func newSeries(d *prometheus.Desc, labels ...string) series {
	s := series{desc: d, n: len(labels)}
	copy(s.labels[:], labels)
	return s
}

func collectCounters(ch chan<- prometheus.Metric, snap Snapshot, counters map[datapath.LiveKey]datapath.CounterValue) {
	countOwners := countOwnerIndex(snap)
	sums := map[series]float64{}
	var mss float64
	for k, v := range counters {
		switch k.Chain {
		case datapath.ChainCount:
			iface := k.Iif
			dir := "request"
			if k.Dir == datapath.CtDirReply {
				iface, dir = k.Oif, "reply"
			}
			key := datapath.DNATKey{Iface: iface, Proto: k.Proto, First: k.Port.First, Last: k.Port.Last, Target: k.Target.Unmap()}
			for _, o := range countOwners[key] {
				sums[newSeries(descPackets, o.Namespace, o.Name, iface, family(k.Family), role(iface), dir)] += float64(v.Packets)
				sums[newSeries(descBytes, o.Namespace, o.Name, iface, family(k.Family), role(iface), dir)] += float64(v.Bytes)
			}
		case datapath.ChainPre:
			key := datapath.DNATKey{Iface: k.Iif, Proto: k.Proto, First: k.Port.First, Last: k.Port.Last, Target: k.Target.Unmap()}
			if o, ok := snap.Index.DNAT[key]; ok {
				sums[newSeries(descConnections, o.Namespace, o.Name, k.Iif, family(k.Family), role(k.Iif))] += float64(v.Packets)
			}
		case datapath.ChainEgress:
			o, ok := snap.Index.Egress[k.Src]
			if !ok {
				continue
			}
			path := "link"
			if k.Mark == datapath.EgressBlockedMark {
				path = "blocked"
			}
			sums[newSeries(descEgressPackets, o.Namespace, o.Egress, o.Pod, family(k.Family), path)] += float64(v.Packets)
			sums[newSeries(descEgressBytes, o.Namespace, o.Egress, o.Pod, family(k.Family), path)] += float64(v.Bytes)
		case datapath.ChainMSS:
			mss += float64(v.Packets)
		}
	}
	for s, v := range sums {
		ch <- prometheus.MustNewConstMetric(s.desc, prometheus.CounterValue, v, s.values()...)
	}
	ch <- prometheus.MustNewConstMetric(descMSS, prometheus.CounterValue, mss)
}

// countOwnerIndex maps a kup-count rule's key to the PortMaps it counts for. A
// kup-count rule matches the target's port, the translated one when the
// mapping translates, so two PortMaps mapping different ports to the same
// target port share one rule and both get its counter.
func countOwnerIndex(snap Snapshot) map[datapath.DNATKey][]reconcile.Owner {
	out := map[datapath.DNATKey][]reconcile.Owner{}
	for _, d := range snap.DNAT {
		k := datapath.KeyOf(d)
		o, ok := snap.Index.DNAT[k]
		if !ok {
			continue
		}
		if d.ToPort != 0 {
			k.First, k.Last = d.ToPort, d.ToPort
		}
		dup := false
		for _, have := range out[k] {
			dup = dup || have == o
		}
		if !dup {
			out[k] = append(out[k], o)
		}
	}
	return out
}

// collectOpenFlows dumps conntrack once and counts each mapping's flows. A
// failed or slow dump counts in kuport_conntrack_dump_errors_total and leaves
// the open-flow series out of this scrape.
func collectOpenFlows(ch chan<- prometheus.Metric, src Source, snap Snapshot) {
	if len(snap.DNAT) == 0 {
		return
	}
	flows, err := src.Flows(context.Background())
	if err != nil {
		ConntrackDumpErrors.Inc()
		return
	}
	sums := map[series]float64{}
	for k, n := range datapath.CountFlows(flows, snap.DNAT) {
		o, ok := snap.Index.DNAT[k]
		if !ok {
			continue
		}
		sums[newSeries(descOpenFlows, o.Namespace, o.Name, role(k.Iface))] += float64(n)
	}
	// A mapping with no open flow reads 0 rather than vanishing.
	for k, o := range snap.Index.DNAT {
		s := newSeries(descOpenFlows, o.Namespace, o.Name, role(k.Iface))
		if _, ok := sums[s]; !ok {
			sums[s] = 0
		}
	}
	for s, v := range sums {
		ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, v, s.values()...)
	}
}

// role is deliver for a rule on a return link, the pod's node of a remote
// mapping, and accept for the interface a client reached.
func role(iface string) string {
	if strings.HasPrefix(iface, "kup-") {
		return "deliver"
	}
	return "accept"
}

func family(f datapath.Family) string {
	if f == datapath.FamilyIPv6 {
		return "ipv6"
	}
	return "ipv4"
}
