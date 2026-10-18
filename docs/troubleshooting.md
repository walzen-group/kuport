# Troubleshooting

For the person holding a mapping or an Egress that is not working. Each section
names the command to run and the answer that tells you what to do next.
[Egress](#egress) covers outbound traffic; every other section is about
PortMaps.

Where this page describes a failure mode, it comes from the design measurements
in [datapath.md](datapath.md), from a condition the code reports, or from what a
live cluster has done. It is not an incident log, and a cluster outage is still
where new failure modes will be found.

## Reading status

The first move is always the same:

```sh
kubectl get portmap -n <namespace> <name> -o wide
```

Expected result: rows with CLASS, PROTO, PORT, ENDPOINT and, with `-o wide`,
PUBLISHED. `published` lists the addresses the port answers on right now: one
row per class interface on the mapping's serving node, the one node where the
DNAT rules exist. That is the thing to hand to whoever calls in from outside.
The serving node writes the mapping's status and reads these addresses off its
own host, so a row can carry a blank address while its interface has not
resolved; the name is complete from the start and the address fills in on a
later pass.

What the shapes tell you:

| What you see | What it means |
| --- | --- |
| A row with a populated `published` | the DNAT rules exist on the serving node for those addresses |
| `Accepted=True`, `Programmed=False` | the mapping is admissible but the serving node has not finished writing what the port needs; read the reason |
| `Accepted=False` | the mapping will never be programmed until the reason is fixed; every reason is a decision the API made about the object |
| An empty status, nothing in `published`, no conditions | no agent has taken the object. Something is wrong below the mapping: check the DaemonSet |

The last row deserves a sentence of its own: an absent condition is
information. Every agent that can see the API computes a status owner for
every PortMap (the mapping's serving node, else the chosen endpoint's node,
else the first accepting node, else the first node), so a PortMap with no
status at all means no agent pod is running, not that the reconcile skipped it.

The class has its own report:

```sh
kubectl get portmapclass <name> -o yaml
```

Read `status.nodes` (one row per node the class selects: ready, message, the
MTU numbers and the interface addresses it resolved) and the Ready and
Deprecated conditions.

The return links are cluster-scoped Link objects, one per node pair a class
needs:

```sh
kubectl get links
```

Each row names a slot, `slot-<n>`, with the class kind, the class, the two
nodes and, for a link no mapping needs, how long ago that became true. Slot n
gives the device `kup-<n>`, a mark of `0x6b70` and the slot in its low bits,
and the routing table 10000 + n; [api.md](api.md#link) lists every number a
slot fixes.

Logs come from the DaemonSet:

```sh
kubectl -n kuport-system logs daemonset/kuport-agent
```

## Conditions and reasons

Every reason the code can write, and the first thing to check for each.

### PortMap conditions

| Reason | On | Status | Meaning | First check |
| --- | --- | --- | --- | --- |
| Valid | Accepted | True | the class exists, the namespace is selected, the port range fits and is unreserved, no earlier mapping holds it | nothing, this is admission |
| ClassNotFound | Accepted | False | `spec.className` names no PortMapClass | `kubectl get portmapclass` |
| NamespaceNotSelected | Accepted | False | this namespace's labels do not satisfy the class `namespaceSelector` | `kubectl get ns <ns> --show-labels` and the class selector |
| PortOutOfRange | Accepted | False | port (or the whole port..endPort range) falls outside the class `ports.min`..`ports.max` | compare the mapping against the class ports block; a range must fit entirely |
| PortReserved | Accepted | False | the range touches a port in the class `ports.reserved` | reserved is the admin keeping ports back; ask, or pick another port |
| PortConflict | Accepted | False | an earlier PortMap on the same class overlaps this range; the earlier one wins by creation time, then name | `kubectl get portmap -A` and compare ports per class |
| InvalidPortRange | Accepted | False | `endPort` is below `port`. The CEL rule on the CRD should have rejected the update, so this reason is a bug report | file it with the object's YAML |
| RangeTranslation | Accepted | False | a range's named port lists a number other than `port` in the EndpointSlice. A range delivers each port as dialed, so the pod would get traffic on ports it does not listen on | `kubectl -n <ns> get endpointslice -l kubernetes.io/service-name=<svc> -o yaml` and the pod's containerPort; make them equal, or map a single port |
| TargetPortInUse | Accepted | False | an earlier mapping uses the same Service port and protocol, and one of the two translates to another port. The message names the earlier mapping | keep one mapping per Service port, or give the second port its own named port on the Service and the pod |
| AllNodesReady | Programmed | True | the serving node has written its rules, its row in the class status reports ready, and a remote pod's return link has landed with addresses at both ends | if the port still fails traffic, the fault is outside the rules: see the host firewall and counters |
| NoReadyEndpoint | Programmed | False | the named port of the referenced Service has no ready endpoint; the DNAT rule is removed, so the port refuses (reset, ICMP unreachable) instead of blackholing | `kubectl -n <ns> get endpointslice -k kubernetes.io/service-name=<svc>`; readiness of the pods |
| ReturnPathUnavailable | Programmed | False | the pod is on another node and its return path is not up: the class mode is None, the pair's Link has not reached the serving node yet (`return link for <pair> is not claimed yet`), or a node on the path has no usable address. A missing Link is a window that closes by itself once the pod's node creates it and the watch delivers it; a None mode is a decision | the class `returnPath` and its Ready condition, then `kubectl get links` for the pair; if the reason repeats beyond a couple of passes, whether the pod's node runs an agent that can create the Link |
| NodeNotReady | Programmed | False | the serving node's row in the class status is missing or reports not ready, or the class selects no accepting node at all. The message names the serving node and what its row said, like `serving node edge-a is not ready: interface wt0 not present` | the DaemonSet pod on that node, the node itself, and the class `nodes[]` row's message |

### PortMapClass conditions

| Reason | Status | Meaning | First check |
| --- | --- | --- | --- |
| Valid | True | the link pool has a slot for every pair the class needs; the port check is overlaid by the agents from the host | nothing, this is the healthy state |
| TunnelModeRequired | False | the agent read the CNI's configuration and it is not tunneling (for Cilium: `routing-mode` is not `tunnel`), so the inbound leg cannot preserve the client address | `kubectl -n kube-system get cm cilium-config -o jsonpath='{.data.routing-mode}'` |
| VxlanPortConflict | False | the agent's link port (`--link-port`, 4790 unless set) equals the CNI's VXLAN port (8472 on Cilium); the links would swallow the CNI's own traffic | set LINK_PORT on the DaemonSet to another port, the same on every node |
| SubnetExhausted | False | `all 1024 slots of the cluster-wide link pool are taken`: 1024 Link objects exist, and a new pair gets none | `kubectl get links`; the agents delete a Link 24 hours after its UNUSED-SINCE, so the pool refills on its own once mappings stop needing links |

The Deprecated condition sits beside Ready and leaves Ready unchanged:

| Reason | Status | Meaning | First check |
| --- | --- | --- | --- |
| NoDeprecatedFields | False | the class sets no field kuport ignores | nothing, this is the healthy state |
| DeprecatedField | True | the class sets `returnPath.vxlan.vni`, `port`, `subnet` or `subnet6`, which kuport ignores; the message names each one | remove the vxlan block from the class. The link port is the agent's `--link-port` |

## The counters

Every rule kuport writes carries a `counter`, so the datapath answers the
question "did the packet get here" without logs. Run `nft list table inet
kuport` in the node's network namespace. The agent image has no `nft`, so use a
node debug pod:

```sh
kubectl debug node/<node> -n kube-system --profile=sysadmin --image=alpine:3.24.2 --attach -- sh -c 'apk add -q nftables && nft list table inet kuport'
```

`kubectl debug` leaves the pod `node-debugger-<node>-<suffix>` in kube-system;
delete it afterwards.

You will see the maps, the four chains and, on each rule, `counter packets N bytes M`.
A mapping delivered over IPv6 has an `ip6` rule beside each `ip` one, each
with its own counter, so the table splits the path by family as well as by
stage. A `table ip kuport` still present means the node runs an agent older
than the inet table; the current agent deletes it on its first pass.
What a climb means, and what a zero means, stage by stage:

| Rule | Counter climbing | Counter stuck at zero |
| --- | --- | --- |
| `kup-steer` (serving node) | packets addressed to one of the node's own addresses arrive at that interface and port and are steered to its stand-in | nothing addressed to a node address arrives on that tuple, or a chain at a priority below -120 drops it: wrong address dialed, upstream routing, or a firewall in raw or mangle. A client dialing an address the node does not hold reaches kup-pre unsteered |
| `kup-pre` DNAT (serving node) | the packet reached the translation | with `kup-steer` climbing, a chain between -120 and -100 drops it: trace it, see the host firewall below. With both at zero, nothing reaches the node on that tuple. Compare `published` against where the client actually sends |
| `kup-post` with `oifname "cilium_host"` (same-node pod) | the inbound leg is being claimed before Cilium's masquerade; the pod should see the client address | with the DNAT counter climbing, the pod is on another node; look at the target node instead |
| `kup-mangle` mark rule (target node) | replies from the pod are marked and policy-routed back to the serving node | the pod's replies do not match src and sport: it replies from another address or port, or it never received the request |
| `kup-post` with `oifname != "cilium_*"` (target node) | the reply leaves through the return link toward the serving node | replies are taking some other route: check the ip rule ladder and the link, next sections |

A packet that never reaches the DNAT counter is a different problem from one
that reaches it and never arrives at the pod. The counters split the path at
exactly that seam, which saves guessing.

## The failure that leaves no log line

The VXLAN routing loop. The vxlan driver copies a packet's mark onto the
encapsulated outer packet. If the kuport loop-guard rule at pref 101 is missing
or overridden, that outer packet matches the divert rule at pref 102 and gets
routed back into the device it just left. The kernel refuses, sends you no
message of any kind, and increments the device's transmit error counter.

Spot it with:

```sh
ip -s link show dev kup-<n>
```

Expected result on a healthy node: RX and TX climbing, TX ERRORS staying flat.
TX ERRORS climbing in step with client probes is the loop. Causes: something
else rewrote `ip rule`, an old agent version that predates the loop guard, or
hand edits. The fix is to let the agent restore its rules (restart its pod);
the reconcile re-adds whatever it finds missing, and removes what it owns but
does not want.

```sh
ip rule list | grep 0x6b70
```

You should see two lines per active link: pref 101 (to the peer's /32, lookup
main) and pref 102 (lookup the slot's table, 10000 + n for `kup-<n>`). One
line per link is the loop.

A link carrying IPv6 adds one IPv6 rule, the pref 102 divert with the same mark
and table:

```sh
ip -6 rule list | grep 0x6b70
```

The IPv6 list has no pref 101 line. The outer packet is IPv4, so the IPv4 loop
guard covers IPv6 replies too.

## IPv6 does not answer

The IPv4 address of a mapping answers and the IPv6 one does not. Read the
mapping's status first:

```sh
kubectl get portmap -n <namespace> <name> -o yaml
```

| What you see | Meaning | First check |
| --- | --- | --- |
| no `endpoint.address6` | the serving node does not deliver this mapping over IPv6: the Service has no IPv6 EndpointSlice, its IPv6 slice does not list the chosen pod as ready, or the serving node does not forward IPv6 | `kubectl -n <ns> get endpointslice -l kubernetes.io/service-name=<svc>`, then the class row below |
| `endpoint.address6` set, the row's `address6` empty | the interface holds no global IPv6 address on the serving node | `ip -6 addr show dev <iface>` on that node |
| both set, IPv6 times out | the rules exist on the serving node; the pod's node may not forward IPv6, or the link may be too small for IPv6 | the agent log on the pod's node, then the link MTU |

A node whose `net.ipv6.conf.all.forwarding` is 0 writes no IPv6 rule, address
or route, and keeps delivering IPv4. Its class status row carries
`ipv6Unavailable` when one of its class interfaces holds an IPv6 address:

```sh
kubectl get portmapclass <name> -o jsonpath='{.status.nodes}'
```

The pod's node is often in no class, so it has no row. Its agent logs the state
once each time it changes:

```sh
kubectl -n kuport-system logs <agent pod on that node> | grep "IPv6 forwarding"
```

Expected result on a node that delivers IPv6: `IPv6 forwarding is on`. The line
`IPv6 forwarding is off; this node delivers IPv4 alone` means the sysctl reads 0
or is missing; Cilium with IPv6 enabled sets it to 1.

A link whose MTU is below 1280 carries no IPv6, because the kernel refuses an
IPv6 address on it. The [MTU](#mtu) section reads the link figures; an
underlay below 1330 produces such a link.

## The host firewall

kuport steers a mapped packet past a firewall that drops new connections to the
node's own addresses, Talos's ingress firewall among them, so such a firewall
needs no rule for a mapped port. [datapath.md](datapath.md#host-firewall) shows
how, and [decisions.md](decisions.md#host-firewall) why.

A firewall that drops forwarded traffic still drops a mapping's packet: ufw's
default forward policy, a firewalld zone without forwarding, or any forward
chain with a drop policy. kuport then reports the mapping fully healthy,
`Accepted=True`, `Programmed=True` and `published` populated, because status
describes the rules kuport owns and not the rest of the machine's. The
`kup-steer` and `kup-pre` counters climb while the pod never sees the request.
Allow forwarding to the pod network with whatever manages that firewall. Nothing
between nodes needs opening: the return link's packets carry the nodes' own
addresses and ride the mesh like any node-to-node traffic.

To find the chain that drops a packet, trace it. This adds a table `kuptrace`
that marks one port for tracing, prints 30 seconds of trace while a client
connects, and deletes the table again:

```sh
kubectl debug node/<node> -n kube-system --profile=sysadmin --image=alpine:3.24.2 --attach -- sh -c 'apk add -q nftables && nft add table inet kuptrace && nft add chain inet kuptrace pre "{ type filter hook prerouting priority -350; }" && nft add rule inet kuptrace pre udp dport <port> limit rate 2/second meta nftrace set 1 && timeout 30 nft monitor trace; nft delete table inet kuptrace'
```

The line ending in `(verdict drop)` names the table and chain that dropped the
packet. [datapath.md](datapath.md#host-firewall) shows a trace of Talos's
firewall dropping one before v0.7.0.

## MTU

The class status carries the arithmetic:

```sh
kubectl get portmapclass <name> -o jsonpath='{.status.nodes}'
```

`underlayMTU` is the MTU of the interface carrying the return links, read from
that node. `linkMTU` is that number minus 50, the VXLAN overhead, and it is
what that node's own path allows.

Each link device is set to the smaller of its two ends' figures, so a link to a
peer on a thinner underlay carries less than either node's `linkMTU` row
suggests. Read the devices when a number has to be exact:

```sh
ip link show type vxlan | grep kup-
```

A margin of zero is not an error. On the cluster this was designed against the
mesh carries 1350 and every link lands on 1300.

A datagram above the link figure still crosses. The forwarding path splits it
and the far side reassembles, measured on 2026-09-08 at reply sizes up to 1576
bytes. Fragmentation puts two packets on the wire for one and loses the whole
datagram when either half is lost, so treat it as what keeps a mistake from
becoming an outage rather than as a size to design for.

Cilium sets a separate limit, and kuport's traffic does not meet it. Cilium
leaves a pod's interface at the underlay MTU and gives the pod's default route
an MTU 50 below it. A datagram to a pod on another node that exceeds the route
MTU is fragmented by the pod's own stack before it enters the tunnel, and
arrives as two packets. Traffic through a mapping never uses that route: a
reply leaves the pod onto its own node's host stack and goes out the return
link, and an inbound packet reaches the pod through the host's forwarding path. A workload that also talks pod to pod meets the limit
there and should keep that traffic under `linkMTU`.

## Egress

A pod an Egress selects should reach the internet from the egress node's
address. Start with the list:

```sh
kubectl get egress -A
```

Expected result: one row per Egress with CLASS, NODE (the egress node, from
status), FAIL-CLOSED and AGE. An empty NODE means the agents have written no
status yet, or the class names no Node.

Then read the Egress:

```sh
kubectl -n <namespace> get egress <name> -o yaml
```

`status.pods` lists every selected pod with its node, its addresses and the
path its node programmed, `path` for IPv4 and `path6` for IPv6:

| Path | Meaning | First check |
| --- | --- | --- |
| Link | the pod's default-route traffic goes over a link to the egress node | if a site still sees the pod's own node, see the table further down |
| Local | the pod runs on the egress node and leaves through the CNI's masquerade there | nothing; the pod already leaves from the egress node |
| Blocked | the path is down and `failClosed: true` drops the pod's default-route traffic | the Programmed reason below |
| Direct | the path is down and the pod leaves from its own node | the Programmed reason below |

A selected pod missing from `status.pods` has no address yet, is not scheduled,
has finished, or belongs to an earlier Egress.

### Egress conditions

| Reason | On | Status | Meaning | First check |
| --- | --- | --- | --- | --- |
| Valid | Accepted | True | the class exists and selects this namespace | nothing |
| ClassNotFound | Accepted | False | `spec.className` names no EgressClass | `kubectl get egressclass` |
| NamespaceNotSelected | Accepted | False | the namespace's labels do not satisfy the class `namespaceSelector` | `kubectl get ns <ns> --show-labels` and the class selector |
| AllPodsRouted | Programmed | True | every selected pod's path is Link or Local | if traffic still leaves from the wrong address, the node checks below |
| EgressNodeNotFound | Programmed | False | the class's `spec.node` names no Node. Every pod's path is down | `kubectl get nodes`, then correct `spec.node` on the EgressClass |
| EgressConflict | Programmed | False | an earlier Egress selects some of these pods (the message names them) and decides their path | `kubectl -n <ns> get egress` and compare the selectors; narrow one of them |
| PodCIDRsUnknown | Programmed | False | a Node has no `spec.podCIDRs`, so the agents cannot build the cluster sets and program no path | the command below; the CNI's IPAM has to fill the field, which Cilium does with `ipam.mode: kubernetes` |
| SubnetExhausted | Programmed | False | all 1024 slots of the link pool are taken | `kubectl get links` |
| ReturnPathUnavailable | Programmed | False | `the path to <node> is down for <pods>: no Link yet, a node not ready, or a class row missing or not ready`: the pair's Link does not exist yet, the egress node or the pod's node has Ready False or Unknown, or the EgressClass has no ready row from one of those two nodes | `kubectl get nodes`, then `kubectl get links`, then `kubectl get egressclass <name> -o jsonpath='{.status.nodes}'`; a row missing for more than a pass or two means that node runs no agent |

When several false reasons apply, Programmed reports the first in table order.
A node without pod CIDRs shows up in:

```sh
kubectl get nodes -o custom-columns=NAME:.metadata.name,PODCIDRS:.spec.podCIDRs
```

Expected result: a CIDR on every row. A row reading `<none>` is the node that
blocks every Egress.

### Traffic leaves from the wrong address

| What you see | Cause | First check |
| --- | --- | --- |
| a site sees the pod's own node, path Link | the destination has a route more specific than main's default, or the pod's node runs a netbird exit node whose default route in table 7120 answers before kuport's rules | `ip route get <site address> mark <link mark>` on the pod's node, and netbird's exit-node setting |
| a site sees the pod's own node for the first seconds after the pod starts | the agent had not yet seen the pod's address; kuport does not close that window | nothing to fix; [datapath.md](datapath.md#when-the-path-is-down) describes it |
| the pod reaches nothing outside the cluster, path Link | the app's CiliumNetworkPolicy denies egress to the world, and Cilium drops the packet before it reaches the host | the policy; it has to allow the pod's egress to the world |
| the pod reaches nothing outside the cluster, path Blocked | the path is down and the Egress fails closed | the Programmed reason |

### Rules on the nodes

Read the routing rules, the blocked table and the kup-egress counters on the
pod's node from a node debug pod:

```sh
kubectl debug node/<node> -n kube-system --profile=sysadmin --image=alpine:3.24.2 --attach -- sh -c 'apk add -q iproute2 nftables && ip rule list && ip route show table 11024 && nft list chain inet kuport kup-egress'
```

Expected result on a pod's node with a pod on path Link: rules at prefs 32763,
32764 and 32765 for the link's mark (`0x6b70` and the slot), the 32764 one
carrying `suppress_prefixlength 0`, and a kup-egress rule for the pod's address
whose counter climbs while the pod opens connections. The route behind 32765 is
in table 10000 plus the slot:

```sh
kubectl debug node/<node> -n kube-system --profile=sysadmin --image=alpine:3.24.2 --attach -- sh -c 'apk add -q iproute2 && ip route show table <10000 + slot>'
```

Expected result: `default via` the egress node's end of the link, `dev kup-<n>`.
`kubectl get links` gives the slot.

A pod on path Blocked has a kup-egress rule setting mark 0x6b70f000, rules at
32764 and 32765 for that mark, and `blackhole default` in table 11024. A pod on
path Direct or Local has no rule at all.

| kup-egress counter | Meaning |
| --- | --- |
| climbing | the pod's outbound packets reach the host and are marked |
| stuck at zero while the pod sends | the packets never reach the host's prerouting hook, or their source is not the address in the rule: the pod's address changed and the agent has not run since, or the CNI routes the pod's traffic outside the host stack |

On the egress node the same command lists rules at prefs 101 and 102 for the
link's mark. `nft list table inet kuport` there shows a `ct mark set` rule and a
`masquerade` rule for each `kup-<n>` an Egress uses, and both counters climb
while the pod's traffic flows.

## Agent lifecycle

What happens to the host state when the agent moves:

| Event | Effect |
| --- | --- |
| Clean shutdown (SIGTERM, rolling update, node drain) | the agent removes its nftables table, its kup- links, its routing rules and routes. A node taken out of a class stops holding state nobody wants |
| Clean shutdown on a node holding a fail-closed Egress pod | the agent leaves those pods blocked: their mark, the blocked rules, the blackhole in table 11024 and the cluster sets stay until the next agent's first pass. Removing kuport leaves them until the node reboots, so delete the Egress objects first |
| Crash (OOM, node power loss) | the rules stay. The next start reconciles: it rewrites the table whole and removes owned objects the new desired state does not name |
| Rolling update of the DaemonSet | `maxUnavailable: 1`, one node at a time. On each node there is a window of seconds where the table is gone and rebuilt: the port refuses during the window, and established connections through that node are dropped, because flushing the table drops their NAT bindings |
| Upgrade from v0.7.1 or earlier | as a rolling update, and each node's first pass rebuilds every link under its Link slot. A link between an upgraded node and an old one does not carry traffic until both run the new agent; [integration.md](integration.md#upgrading-from-v071-or-earlier) has the steps |
| Endpoint pod reschedules | every agent recomputes the choice, so the DNAT target changes on the serving node's next pass and another accepting node may take over serving. Old connections do not carry over; the port refuses for the gap and serves again once the new endpoint is ready |

The rollout interruptions are the accepted cost of the NoReadyEndpoint
decision: refuse fast, never blackhole. If your workload cannot tolerate a
few refused seconds per node per release, that is the number to design around,
not kuport's bug.
