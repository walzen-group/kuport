# Metrics

Each kuport agent serves `/metrics` on its node's InternalIP, port 9790. The
series describe what the agent programmed on its own node: traffic through each
PortMap, the flows its DNAT rules made, Egress and MSS counters, the node's
return links, and the agent's own applies. A Prometheus that scrapes every agent
sees the whole cluster.

## Where it listens

| Flag | Variable | Default | Values |
| --- | --- | --- | --- |
| --metrics-bind | METRICS_BIND | empty: the node's InternalIP, read from HOST_IP | an interface name such as wt0, or off |
| --metrics-port | METRICS_PORT | 9790 | a TCP port |

The DaemonSet passes the InternalIP through the downward API and names the port:

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

The agent runs with `hostNetwork: true`, so a wildcard listener would open the
endpoint on every address the node holds, a public uplink included. The agent
never opens one. With the default it listens on `<InternalIP>:9790` only. With
an interface name it opens one listener per address on that interface, IPv4 and
IPv6, and skips link-local addresses. The flag accepts an interface name only:
with an address such as `0.0.0.0` in it, the agent logs the error and the
endpoint stays closed.

The endpoint serves plain HTTP with no authentication, as node-exporter does.
The node's firewall decides who reaches it.

A missing address never stops the datapath. When the interface does not exist,
holds no usable address, or the bind fails, the agent logs
`metrics endpoint not listening; retrying` once with the error, and tries again
every 30 seconds while it keeps reconciling. Once it binds, it logs
`metrics endpoint listening` with the address.

`/metrics` serves controller-runtime's registry, so the controller's own
series, such as `controller_runtime_reconcile_time_seconds`, come with it.

## Scraping

A PodMonitor selects the agent pods by their label:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: kuport
spec:
  namespaceSelector:
    any: true
  selector:
    matchLabels:
      app.kubernetes.io/name: kuport
  podMetricsEndpoints:
  - port: metrics
    honorLabels: true
    relabelings:
    - sourceLabels: [__meta_kubernetes_pod_node_name]
      targetLabel: node
```

Every traffic series carries the namespace of the PortMap or Egress it
describes. Without `honorLabels`, Prometheus replaces that label with the
agent's own namespace and moves the value to `exported_namespace`. The
relabeling adds `node`, because each agent counts only its own node's rules.

## Series

| Series | Type | Labels | Read from |
| --- | --- | --- | --- |
| kuport_mapping_info | gauge, 1 | namespace, portmap, class, mode, protocol, port, endpoint_node | the reconcile, for each mapping this node holds a DNAT rule for |
| kuport_mapping_packets_total, kuport_mapping_bytes_total | counter | namespace, portmap, interface, family, role, direction | kup-count |
| kuport_mapping_connections_total | counter | namespace, portmap, interface, family, role | kup-pre, which sees only the first packet of each flow |
| kuport_mapping_open_flows | gauge | namespace, portmap, role | the node's conntrack table |
| kuport_egress_packets_total, kuport_egress_bytes_total | counter | namespace, egress, pod, family, path (link, blocked) | kup-egress |
| kuport_mss_clamped_total | counter | none | kup-mss |
| kuport_link_info | gauge, 1 | device, slot, class, peer | the Links this node holds a device for |
| kuport_apply_total | counter | result (ok, error) | each datapath apply |
| kuport_last_apply_success_timestamp_seconds | gauge | none | each successful apply |
| kuport_state_entries | gauge | kind (dnat, exempt, mark, link, rule, route) | the numbers in the `applied datapath state` log line |
| kuport_conntrack_dump_errors_total | counter | none | a conntrack dump that failed or ran past 5 seconds |

The series stop at the mapping. The agent exports nothing per client.

Return-link bytes and errors come from node-exporter, which reads every device,
`kup-<n>` included. Join `kuport_link_info` to `node_network_transmit_bytes_total`
on the node and the device.

## role and direction

A mapping whose pod runs on another node crosses two nodes, and both agents
count it. Every traffic series carries `role`, read from the interface the rule
names:

| role | Interface | Node | Counts |
| --- | --- | --- | --- |
| accept | wt0, enp1s0, any interface a class lists | the node the client reached | traffic as the client sent and received it |
| deliver | `kup-<n>` | the pod's node of a remote mapping | traffic that crossed the return link |

`direction` is `request` for packets the client sent and `reply` for packets the
pod sent back. A sum over `role="accept"` counts each client's traffic once.
Requests rising under `accept` on the accepting node while `deliver` stays flat
on the pod's node mean the return link loses them. Under a Multi class,
`deliver` splits by `kup-<n>`, one link per accepting node.

Here is the collector's output for a node that accepts a jellyfin mapping on wt0,
translating port 10002 to 8096, and delivers a teamspeak mapping arriving over
kup-0 from another node. TestCollect in internal/metrics/collector_test.go
checks these lines, among the rest of the scrape, against the collector:

```text
kuport_mapping_info{class="overlay",endpoint_node="node-a",mode="Multi",namespace="media",port="10002",portmap="jellyfin",protocol="tcp"} 1
kuport_mapping_info{class="public",endpoint_node="node-b",mode="Single",namespace="games",port="9987",portmap="teamspeak",protocol="udp"} 1
kuport_mapping_packets_total{direction="reply",family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 8
kuport_mapping_packets_total{direction="request",family="ipv4",interface="kup-0",namespace="games",portmap="teamspeak",role="deliver"} 5
kuport_mapping_packets_total{direction="request",family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 10
kuport_mapping_connections_total{family="ipv4",interface="wt0",namespace="media",portmap="jellyfin",role="accept"} 2
kuport_mapping_open_flows{namespace="games",portmap="teamspeak",role="deliver"} 1
kuport_mapping_open_flows{namespace="media",portmap="jellyfin",role="accept"} 1
kuport_link_info{class="public",device="kup-0",peer="node-c",slot="0"} 1
```

`port` in `kuport_mapping_info` is the port the client dials, `27015-27115` for
a range.

## How kup-count counts

kup-count is the one chain the metrics add to `table inet kuport`. It holds two
rules per DNAT rule, one per direction, and returns no verdict. From the
`accepting-single-port` golden:

```
chain kup-count {
	type filter hook postrouting priority filter; policy accept;
	iifname "enp1s0" ip daddr 10.244.18.107 udp dport 3000 ct direction original ct status dnat counter
	oifname "enp1s0" ip saddr 10.244.18.107 udp sport 3000 ct direction reply ct status dnat counter
}
```

The DNAT rule writes the target's address and port onto a request in
prerouting. The kernel writes the dialed address back onto a reply in
postrouting at srcnat (100). kup-count runs at filter (0) in postrouting,
between the two, so both directions still carry the target's address and port
in their headers. At srcnat + 10 the reply rule would count nothing, which the
kernel test shows by moving the chain there.

`ct status dnat` keeps out a flow that reaches the pod's address and port with
no DNAT on the way. The interface match keeps out traffic to the same pod that
arrives by another interface.

The rules match the target's port, the translated one when the mapping
translates. Two PortMaps on one interface that map different dialed ports to the
same pod and port, 10002 and 10003 both to 8096, render the same pair of rules,
and each PortMap's series reports the traffic of both.

`kuport_mapping_connections_total` comes from the counter on the kup-pre rule,
which the kernel runs for the first packet of each flow only. The accepting rule
matches any destination on its interface and port, so a flow dialed straight to
the pod's address on that interface counts as a connection there even though
kup-count leaves it out.

## Counters across applies

Each apply deletes and re-adds `table inet kuport` in one transaction, and the
agent applies on every pass, which any watched change in the cluster starts.
Every nftables counter restarts at zero each time.

Before each apply, the agent reads the live counters and adds them to running
totals it holds in memory. A scrape reports the totals plus what the live table
has counted since. One lock covers the read, the apply and every scrape, so a
scrape never counts the old table twice. The totals reset only when the agent
restarts, which Prometheus handles like any exporter restart.

The kernel test in internal/datapath/metrics_kernel_test.go sends five UDP
exchanges, applies the same plan again, and sends three more. The live request
counter reads 0 right after the apply, and the scrape reads 8 packets each way.

## Open flows

On each scrape the agent dumps the node's conntrack table once over netlink and
counts the flows each DNAT rule made: protocol and original destination port
inside the rule's selection, and a reply source equal to the rule's target. A
reply source that differs from the original destination is what a DNAT leaves
in conntrack. On a pod's node under Multi, which can hold an accepting rule and
a link rule for the same pod, a flow to the link's own end counts as `deliver`
and every other flow as `accept`.

The dump stops after 5 seconds. A dump that fails or runs over adds one to
`kuport_conntrack_dump_errors_total`, and that scrape has no
`kuport_mapping_open_flows`. Counting 50000 flows against 20 rules took 1.1 ms in
BenchmarkCountFlows50k on a Ryzen 7 5800X3D; the dump itself is not part of that
number.
