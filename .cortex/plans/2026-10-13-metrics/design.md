# Metrics and a Grafana dashboard

Status: agreed with the admin on 2026-10-13 in conversation. One kuport release
(v0.9.0) plus four infra changes, listed under Infra.

## Goal

The admin opens one Grafana board to answer three questions:

1. Is kuport healthy right now?
2. Who uses which mapped port, and how much?
3. Why does a mapping not answer?

Each agent exports what it programmed on its own node: per-mapping request and
reply counters, new connections, open flows, egress counters, link identities
and its own apply state. kube-state-metrics reports the cluster-wide object
state. The board joins both.

## Endpoint

| Flag | Env | Default | Values |
| --- | --- | --- | --- |
| `--metrics-bind` | METRICS_BIND | empty: the node's InternalIP, from HOST_IP | an interface name such as `wt0`, or `off` |
| `--metrics-port` | METRICS_PORT | 9790 | a port |

The DaemonSet passes the InternalIP through the downward API:

```yaml
env:
  - name: HOST_IP
    valueFrom:
      fieldRef:
        fieldPath: status.hostIP
ports:
  - name: metrics
    containerPort: 9790
```

The agent runs with `hostNetwork: true`, so a wildcard bind would listen on
every node address, a public uplink included. The agent never binds a wildcard.
With the default it listens on `<InternalIP>:9790` only; on the cluster this
was designed against that is the netbird address on wt0. With an interface name
it opens one listener per global address on that interface, IPv4 and IPv6. The
admin's reason: all cluster communication runs over the overlay, so nothing
needs the endpoint on a public interface.

The endpoint serves plain HTTP with no authentication, as node-exporter does on
`[$(HOST_IP)]:9100`. The bind limits it to the overlay, and the Talos firewall
rule opens it to the overlay and pod subnets only.

A missing address does not stop the agent. When the interface is absent or has
no global address, or the bind fails, the agent logs it once, retries every 30
seconds, and keeps reconciling the datapath. Port forwarding never waits on
metrics.

The handler serves controller-runtime's `metrics.Registry`, so the
controller's reconcile metrics come with it. The controller-runtime metrics
server stays off (`BindAddress: "0"`); the agent runs its own listeners because
controller-runtime binds a single address.

Port 9790 was free on all six nodes on 2026-10-13 (`/proc/net/tcp` and
`/proc/net/tcp6` read through talosctl).

## Which object a rule serves

The admin's requirement: metrics read what kuport programs and leave the
existing rules and State as they are. Rules carry no comment.

The reconcile returns a `MetricsIndex` beside the State. It reruns the
existing `emitMapping` per mapping and `emitEgressPodNode` per Egress pod into
a scratch State and records which PortMap produced each DNAT entry, keyed by
interface, protocol, port range and target, and which Egress and pod produced
each egress entry, keyed by source address. At scrape time the agent lists
`table inet kuport`, parses each kup-pre, kup-count and kup-egress rule back
into those keys, and looks up the owner. A kup-count rule matches the target's
port, so the index records each DNAT entry's target port range beside the
dialed one. A live rule with no owner, from a table the latest index does not
describe yet, is skipped for that scrape.

A kup-pre rule on the pod's node matches the stand-in of its link end, which
Render writes, so the key leaves the rule's destination address out; the
interface (`kup-<n>`) already tells the links apart.

## Counters across applies

`applyNFT` deletes and re-adds `table inet kuport` in one transaction, and the
agent applies on every pass, which any watched change in the cluster starts, an
EndpointSlice update included. Every nftables counter therefore restarts at
zero several times a minute.

Before each Apply the agent lists the live table's rules and adds each rule's
packets and bytes to a running total held in memory, keyed by the series the
rule feeds (chain, interface, protocol, port range, target, family, direction). A scrape reports the
running total plus what the live table has counted since the last Apply. The
series reset only when the agent restarts, which Prometheus treats like any
exporter restart. Packets arriving between the read and the table swap, a
window of microseconds, go uncounted.

Apply keeps rewriting the whole table every pass: it also repairs a table
someone flushed by hand, and skipping unchanged passes would lose that.

## Reply counter

No rule counts a mapping's packets both ways. kup-steer's counter sits after
its map lookup, so it misses a request to a routed virtual address under Multi,
and render.go writes kup-steer rules only for a family with steer pairs. A new
chain, kup-count, counts both directions, two rules per DNAT rule:

For a mapping of TCP 10002 to port 8096 on 10.244.18.186:

```
chain kup-count {
	type filter hook postrouting priority filter; policy accept;
	iifname "enp1s0" ip daddr 10.244.18.186 tcp dport 8096 ct direction original ct status dnat counter
	oifname "enp1s0" ip saddr 10.244.18.186 tcp sport 8096 ct direction reply ct status dnat counter
	...two per DNAT rule: one per interface per family per mapping...
}
```

| Match | Why |
| --- | --- |
| `iifname` on the request, `oifname` on the reply | the DNAT rule's interface, the one clients reach the node on, so both directions carry the same interface label |
| `ip daddr`, `dport` on the request; `ip saddr`, `sport` on the reply | the DNAT rule's target address and port (the translated port when the mapping translates), which both directions carry in their header at this priority |
| `ct direction`, `ct status dnat` | one direction of a translated flow; a flow that reaches the pod's address with no DNAT stays uncounted |

The kernel writes the dialed address and port back onto a reply in the nat
hook at srcnat (100). kup-count runs at filter (0) in postrouting, before that
hook, so a reply still carries the target's address and port. At srcnat + 10
the reply rule reads 0; the kernel test's mutation shows it. The chain counts
packets that leave the node: requests forwarded toward the pod or the link,
replies forwarded toward the client. It returns no verdict and changes no
packet. On the pod's node the same pair counts traffic over `kup-<n>`.

Two PortMaps on one interface that map different ports to the same target
address and port (10002 and 10003, both to 8096) render identical kup-count
rules, and each PortMap's series reports the traffic of both. The admin
accepted this on 2026-10-13.

A match on the dialed port through `ct original proto-dst` would tell those two
apart. google/nftables v0.3.0 encodes the conntrack direction attribute that
match needs as 4 bytes where the kernel uses 1, and reading such a rule back
fails with `attribute 3 is not a uint32; length: 1`. Upstream fixed it in
8a8ad2be on main, after the last tag.

## Metrics

Each agent reports only rules on its own node. Prometheus adds `node` through
the PodMonitor.

| Metric | Type | Labels | Source |
| --- | --- | --- | --- |
| `kuport_mapping_info` | gauge, 1 | namespace, portmap, class, mode, protocol, port, endpoint_node | the agent's inputs |
| `kuport_mapping_packets_total`, `kuport_mapping_bytes_total` | counter | namespace, portmap, interface, family, role, direction | kup-count |
| `kuport_mapping_connections_total` | counter | namespace, portmap, interface, family, role | kup-pre, which sees a flow's first packet only |
| `kuport_mapping_open_flows` | gauge | namespace, portmap, role | conntrack, see below |
| `kuport_egress_packets_total`, `kuport_egress_bytes_total` | counter | namespace, egress, pod, family, path (link, blocked) | kup-egress |
| `kuport_conntrack_dump_errors_total` | counter | none | a conntrack dump that failed or ran past 5 seconds |
| `kuport_mss_clamped_total` | counter | none | kup-mss |
| `kuport_link_info` | gauge, 1 | device, slot, class, peer | the Links this node is a peer of |
| `kuport_apply_total` | counter | result (ok, error) | each datapath apply |
| `kuport_last_apply_success_timestamp_seconds` | gauge | none | each successful apply |
| `kuport_state_entries` | gauge | kind (dnat, exempt, mark, link, rule, route) | the numbers in the `applied datapath state` log line |

Link bytes and errors come from node-exporter, which collects every device:
the chart's DaemonSet sets no netdev exclude. The board joins
`kuport_link_info` with `node_network_transmit_bytes_total{device="kup-0"}`, so
no device is counted by two exporters.

### role

A remote mapping crosses two nodes, and both agents count it. Summing over
nodes would double the traffic, so every traffic series carries `role`:

| role | Node | interface | Counts |
| --- | --- | --- | --- |
| `accept` | accepting node | wt0, ens3, enp1s0 | traffic as clients sent and received it |
| `deliver` | pod's node, mapping remote | kup-n | traffic that crossed the return link |

Traffic panels sum `role="accept"`. `deliver` is for debugging a link: requests
under `accept` on the accepting node with a flat `deliver` on the pod's node
mean the link loses them. In a Multi class, `deliver` splits by `kup-n`, one per
accepting node.

### Open flows

On each scrape the agent dumps the conntrack table once over netlink and counts
the flows a DNAT rule made: protocol and original destination port inside the
rule's port selection, reply source equal to its target. netlink v1.3.1's
ConntrackFlow carries no status field; a reply source that differs from the
original destination is what DNAT means. A pod's node under Multi can hold an
accepting rule and a link rule for the same pod and port; a flow whose original
destination is a link's own end counts as `deliver`, every other flow as
`accept`. The test records the scrape time against a
table of 50000 entries. The agent caps the dump at 5 seconds, counts a dump that
fails or runs over in `kuport_conntrack_dump_errors_total`, and leaves
`kuport_mapping_open_flows` out of that scrape.

## Infra

Four changes in the infra repo, in this order. The talos and kube-prometheus
units are applied by the admin; the kuport bump follows the session rule in
CLAUDE.md.

| Order | Unit or path | Change |
| --- | --- | --- |
| 1 | modules/infra/talos/opentofu/patches/firewall.tf | rule `kuport-metrics`, tcp 9790, sources overlay and pod subnets, the same as `node-exporter` |
| 2 | environments/prod/networking/kuport | `kuport_version: "v0.9.0"` |
| 3 | modules/cluster/kube-prometheus | PodMonitor kuport (selects `app.kubernetes.io/name: kuport`, port `metrics`, honorLabels for the `namespace` label) and a kube-state-metrics CustomResourceState block for PortMap, Egress, EgressClass and Link conditions, with RBAC for kuport.wlz.li |
| 4 | flux/templates/management/grafana/base/dashboards/kuport.json | the board |

The PodMonitor needs honorLabels for the same reason as backup-controller's:
each series names the namespace it describes, and Prometheus would otherwise
overwrite `namespace` with kuport-system.

kube-state-metrics series to build on:

| Series | Answers |
| --- | --- |
| `kube_customresource_kuport_portmap_condition{namespace, name, type, status}` | PortMaps not Programmed |
| `kube_customresource_kuport_egress_pod_path{namespace, name, pod, path}` | Egress pods Blocked |
| `kube_customresource_kuport_link_info{name, class}` | Links in use out of 1024 |

## Dashboard

The board lives in the infra repo, at
flux/templates/management/grafana/base/dashboards/kuport.json, next to the
other boards Grafana's sidecar loads. The kuport repo ships no dashboard.

Variables: `class`, `namespace`, `portmap`, `node`.

| Row | Panels |
| --- | --- |
| Health | stats: agents up (`up{job="monitoring/kuport"}`), oldest last successful apply, apply errors in the last hour, PortMaps not Programmed, Egress pods Blocked, Links in use out of 1024 |
| Mappings | a node graph of every mapping's path, then a table with one line per PortMap: class, mode, port, endpoint node, requests/s, replies/s, new connections/s, open flows. A line turns red when requests arrive and no replies leave |
| Traffic | bytes/s and connections/s per mapping (`role="accept"`), requests per accepting node for Multi classes |
| Return links | a table of kup-n with class, peer, bytes each way and tx_errors, and a time series per link |
| Egress | bytes/s per Egress and pod, blocked packets, SYNs clamped |
| Agent | applies and errors per node, reconcile duration, state entries per node |

The node graph is an additional panel; the table and every other row stay. It
draws `interface on node → portmap → kup-n → pod's node` for a remote mapping,
`interface on node → portmap → node` for a local one, and `pod → kup-n → egress
node` for an Egress, each edge showing bytes/s. PromQL builds the `id`, `source`
and `target` columns with `label_replace` and `label_join`, one table-format
query per edge type. If Grafana cannot merge those into the frame the Node
graph panel wants, the agent exports a `kuport_edge{source, target}` series
shaped for it instead. Grafana places the circles itself.

## Tests

| Test | What it checks |
| --- | --- |
| goldens | existing lines unchanged; kup-count renders two rules per DNAT rule; `nft -c` accepts each ruleset |
| collector unit test | rules built with nftExprs and parsed back, with their counters, and a canned conntrack dump turn into the series above, with `role` and `direction` right for an accepting node and a pod's node |
| kernel test | in the existing netns harness, UDP and TCP to a local and a remote mapping: both kup-count rules rise by the packets sent each way, `deliver` rises on the pod's node, and a flow dialed straight to the pod's address and port, with no DNAT, does not move kup-count |
| counters across applies | in the netns harness: traffic, Apply, more traffic, scrape; the series holds both rounds |
| bind test | the default binds HOST_IP only; an interface name binds its global addresses; a missing interface leaves the datapath reconciling and the listener retrying |
| conntrack scale | dump and count 50000 entries; record the time |

## Not in scope

- Alerting rules. They come after the board shows what normal looks like.
- Per-flow or per-client series. The labels stop at the mapping.
- Exporting cluster-wide object state from the agents; kube-state-metrics reads it.
