# Egress through an accepting node

Status: agreed with the admin on 2026-10-11 in conversation: a separate Egress
kind, pods selected by `podSelector`, failing closed. Not built yet.

## Goal

A pod's outbound connections leave the cluster from an accepting node's address.
The first user is the infra repo's qBittorrent on quasar's worker. It needs its
tracker announces, its outbound peer connections and mousehole's API calls to
leave from bar's public address, the address its PortMaps on `netcup-bar-w-1`
publish. Today a PortMap carries only connections a client opens and the
replies to them. Under `Single`, kup-mangle marks `ip saddr <pod ip> <proto>
sport <port>`, so a connection the pod opens from an ephemeral port goes out
Cilium's masquerade from quasar's own address.

## API

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: Egress
metadata:
  name: qbittorrent
  namespace: media
spec:
  className: netcup-bar-w-1
  podSelector:
    matchLabels:
      app.kubernetes.io/name: qbittorrent
status:
  node: talos-netcup-bar-w-1
  pods:
    - name: qbittorrent-0
      node: talos-quasar-w-1
      address: 10.244.5.5
      address6: fd9f:6e92:f8f3:5::5
      path: Link        # Link, Local or Blocked
  conditions: [...]
```

| Field | Meaning |
| --- | --- |
| spec.className | the class whose accepting node carries the traffic. Immutable, for the reason PortMapSpec gives |
| spec.podSelector | pods in the Egress's namespace. Validation rejects an empty selector, which would select every pod in the namespace |
| status.node | the egress node, where the traffic leaves the cluster |
| status.pods | every selected pod with an address, and which path its node programmed for it |

The Egress is independent of PortMaps. A pod can leave through bar with no
inbound port, and a PortMap gains no field. The class's `namespaceSelector`
gates an Egress the way it gates a PortMap; `ports` and the interface lists play
no part.

Conditions reuse the PortMap reasons where they fit: `ClassNotFound`,
`NamespaceNotSelected`, `ReturnPathUnavailable`, `SubnetExhausted`. Two new
ones:

| Reason | When |
| --- | --- |
| `EgressConflict` | a selected pod is selected by an earlier Egress. The earliest `creationTimestamp`, then the name, wins, the order conflicts.go already uses for ports |
| `PodCIDRsUnknown` | a Node has no `spec.podCIDRs`, so the agents cannot build the set of cluster addresses. Every selected pod is Blocked |

## Egress node

The first accepting node of the class by name, the rule a PortMap uses when its
endpoint is on no accepting node. A selected pod on that node needs no rules:
Cilium's masquerade on bar already writes bar's address. Its path is Local.

The Egress node writes the Egress status, as the serving node writes a
PortMap's. With no accepting node in the class, status falls to the first node
in the cluster by name.

## Link

`neededPairsByClass` in internal/reconcile/links.go gains a pair for every
selected pod off the egress node: `pairKey(egress node, pod node)`. The key is
the one a PortMap in the same class produces for a pod on the same node, so
qBittorrent's PortMaps and its Egress share one link, one slot, one mark and one
table. An Egress with no PortMap brings the link up itself. The agent on the
pod's node proposes the claim, as it does for a mapping's endpoint.

## Rules on the pod's node

A new chain, after kube-proxy's DNAT:

```
table inet kuport {
  set kup-cluster4 { type ipv4_addr; flags interval; elements = { <pod cidrs>, <node internal ips> } }
  set kup-cluster6 { type ipv6_addr; flags interval; elements = { <pod cidrs>, <node internal ips> } }

  chain kup-egress {
    type filter hook prerouting priority dstnat + 10; policy accept;
    # path Link
    meta nfproto ipv4 ip saddr <pod ip> ip daddr != @kup-cluster4 ct direction original counter meta mark set <link mark>
    # path Blocked
    meta nfproto ipv4 ip saddr <pod ip> ip daddr != @kup-cluster4 ct direction original counter drop
    ...the ip6 forms with @kup-cluster6...
  }
}
```

The chain sits at `dstnat + 10` because kube-proxy translates a ClusterIP in nat
prerouting at -100. At kup-mangle's priority, `mangle + 10`, the destination of
a DNS query is still the CoreDNS ClusterIP, which no pod CIDR contains, and the
query would go to bar. After -100 it is the CoreDNS pod's address. The kernel
routes after the whole prerouting hook, so a mark set at -90 still selects the
pref 102 rule. The infra repo runs kube-proxy (`kube_proxy_replacement: false`);
with Cilium's socket load balancer the ClusterIP never reaches netfilter and the
priority makes no difference.

`ct direction original` keeps the rule to connections the pod opened. A reply to
a client that reached the pod through another class on the pod's own node keeps
its reply direction and leaves as it does today.

kup-cluster4 and kup-cluster6 hold every Node's `spec.podCIDRs` and its
InternalIP addresses. Traffic to a netbird peer address is outside both sets and
leaves through the egress node like any other destination.

The pod's node needs no masquerade exemption for this traffic. kup-post already
claims `oifname != "cilium_*" ip saddr <pod ip> sport <pod port>`, and the
egress flows need the same claim without the port:

```
oifname "kup-<link>" ip saddr <pod ip> counter snat ip to ip saddr
```

## Rules on the egress node

```
chain kup-mangle {
  ...
  iifname "kup-<link>" ct direction original counter ct mark set <link mark>
  ct direction reply ct mark <link mark> counter meta mark set ct mark
}

chain kup-post {
  ...
  iifname "kup-<link>" ct direction original counter masquerade
}
```

```
ip rule  add pref 101 fwmark <mark> to <pod node address>/32 lookup main
ip rule  add pref 102 fwmark <mark> lookup <table>
ip route add default via <pod node's link address> dev <link> table <table>
```

These mirror the pod node's rules for a reply under `Multi`: the first packet
of a flow from the link writes the link's mark into conntrack, and a reply
arriving on ens3 restores it after conntrack. The nat chain de-masquerades the
reply's destination back to the pod at -100, and the pref 102 rule then routes
it into the link. A reply to a request that arrived on ens3 has a ct mark of 0
and keeps today's path.

The egress node renders these rules for a link only while an Egress selects a
pod on the link's other node. The masquerade matches no address, because the
pod node marks only selected pods. The existing `Single` mark rule (`sport <port>`) also sends a pod's flow
from its mapped port to the egress node; under this design that flow leaves from
the egress node's address, where today it leaves with the pod's address.

### kup-steer needs `ct direction original`

`masquerade` keeps the pod's source port when it is free on bar. A UDP flow
qBittorrent opens from 40000 then leaves bar from 40000, and its replies arrive
on ens3 with `udp dport 40000`. kup-steer matches `iifname "ens3" udp dport
40000` with no conntrack state and would rewrite their destination to a
stand-in. Adding `ct direction original` to every kup-steer rule leaves a
client's packets untouched, since a client always sends in the original
direction, and skips the replies to an egress flow. kup-pre is a nat chain and
sees only a flow's first packet, so it needs no change.

## MSS clamping

The link MTU is the underlay less 50, 1300 on the infra cluster, and Cilium
gives the pod the underlay's 1350. A TCP connection the pod opens would offer an
MSS the link cannot carry. Both ends of every link clamp a SYN leaving by it:

```
chain kup-mss {
  type filter hook forward priority mangle; policy accept;
  oifname "kup-*" tcp flags syn / syn,rst counter tcp option maxseg size set rt mtu
}
```

The pod's node clamps the pod's SYN, and the egress node clamps the remote
SYN-ACK. The clamp also covers PortMap flows, which today depend on
fragmentation or ICMP for TCP above 1300.

## Fail closed

The pod's node chooses Link or Blocked for each selected pod from inputs every
agent shares, so the egress node writes the same path into status:

| Inputs | Path |
| --- | --- |
| the claim for the pair has landed, and the class status rows of the egress node and the pod's node are both ready | Link |
| the claim has not landed, or either row is not ready | Blocked |
| the pod has an IPv6 address and the link has no /127, or either node has IPv6 forwarding off | IPv6 Blocked, IPv4 decided by the rows above |
| a Node lacks `spec.podCIDRs` | Blocked |
| a later Egress selects the pod too | the earlier Egress decides |

The remaining failures and what the pod sees:

| Event | Effect |
| --- | --- |
| egress node powered off | nothing rewrites its row, so the pod node keeps marking. The link sends to a node that is down, and the packets are lost |
| egress node's agent restarts | its Teardown removes the link and the masquerade. VXLAN packets from the pod node reach no device and are dropped until the next agent's first pass |
| pod node's agent shuts down cleanly | Teardown replaces the table with one holding kup-egress and only its drop rules, for every selected pod on the node. The next agent's first pass rewrites the table whole |
| pod node's agent crashes | the rules stay, as for every other rule |
| the Egress is deleted | its rules go on the next pass, and the pod leaves from its own node. Deleting the Egress is the admin's choice to stop routing through bar |
| a new pod starts | the pod sends from the moment its sandbox has an address. Until the agent's Pod watch delivers that address and the next pass runs, no rule matches it and it leaves from its own node |

The last row is a window this design does not close. Neither the gap between a
sandbox getting its address and the agent's next pass nor the time qBittorrent
spends before its first announce has been measured.

Removing kuport while an Egress exists leaves the drop rules in place on every
pod node until that node reboots. Delete the Egress objects first.

## Inputs

| Input | Source | Read by |
| --- | --- | --- |
| Egress | new watch | every agent |
| Pod | new watch, every agent, every namespace. A cache transform keeps the namespace, name, labels, `spec.nodeName`, `status.podIPs` and `deletionTimestamp`, and drops the rest. Pods in phase Succeeded or Failed are skipped | the pod's node for its rules, the egress node for status, every agent for `neededPairsByClass` |
| Node `spec.podCIDRs` | existing Node watch | every agent, for kup-cluster4 and kup-cluster6 |

The infra cluster runs Cilium with `ipam.mode: kubernetes`, which fills
`spec.podCIDRs`. Cilium's cluster-pool mode leaves it empty, and every Egress
then reports `PodCIDRsUnknown`.

The ClusterRole gains `get`, `list` and `watch` on pods and egresses, and
`update` and `patch` on egresses/status.

## Requirements of the cluster

The pod's outbound packets have to pass through the host's prerouting hook on
the pod's node, the same requirement the reply marking already has. The infra
repo's Cilium sets `bpf.hostLegacyRouting: true`. The app's
CiliumNetworkPolicy has to allow the pod's egress to the world; kuport changes
the route the packets take and leaves policy to Cilium.

## Not in scope

- Closing the window before the agent has seen a new pod's address.
- Selecting pods through a Service.
- Per-destination exceptions, such as keeping the netbird range on the pod's
  own node.
- Spreading one Egress over several accepting nodes.

## Tests

- Goldens: kup-egress in both families with the Link and Blocked forms, the
  kup-cluster sets, the egress node's ct save, ct load and masquerade rules,
  kup-mss, and kup-steer with `ct direction original`.
- Reconcile: an Egress whose pod shares a node with a PortMap's endpoint yields
  one link pair; a pod on the egress node yields no rules and path Local; a
  pending claim yields Blocked; two Egresses on one pod resolve to the earlier;
  an empty `spec.podCIDRs` yields `PodCIDRsUnknown` and Blocked.
- Teardown: with a selected pod on the node, Teardown leaves a table holding
  only kup-egress drop rules; with none, it removes the table as today.
- Netlink: the egress node's pref 101 and 102 rules and its table route.
- On the cluster: a probe pod on quasar's worker with an Egress on
  `netcup-bar-w-1` fetches its public address from an address echo service and
  reads bar's. TCP and UDP, IPv4 and IPv6. Then, with bar's agent stopped, the
  probe's fetch times out and never returns quasar's address.
