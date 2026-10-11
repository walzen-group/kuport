# API

Group `kuport.wlz.li`, version `v1alpha1`. Five kinds:

| Kind | Scope | Written by | For |
| --- | --- | --- | --- |
| [PortMapClass](#portmapclass) | cluster | the admin | which nodes accept inbound traffic, on which interfaces and ports |
| [PortMap](#portmap) | namespaced | the workload | one port delivered to the pod behind a Service |
| [EgressClass](#egressclass) | cluster | the admin | the node whose address selected pods' outbound traffic leaves from |
| [Egress](#egress) | namespaced | the workload | which pods in its namespace leave through that node |
| [Link](#link) | cluster | the agents | one return link between two nodes, numbered from a cluster-wide pool |

## PortMapClass

Cluster-scoped. The admin writes it. It says which nodes accept traffic, on
which interfaces, which ports may be asked for, and which namespaces may ask.

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: PortMapClass
metadata:
  name: public
spec:
  nodes:
    worker-1:
      interfaces:
        - wt0
        - enp1s0
    worker-2:
      interfaces:
        - wt0
        - eth0
  ports:
    min: 1024
    max: 65535
    reserved: [80, 443]
  namespaceSelector:
    matchLabels:
      kuport.wlz.li/public: allowed
  returnPath:
    mode: Vxlan
```

| Field | Type | Meaning |
| --- | --- | --- |
| nodes | map of node name to object | Nodes that may accept traffic for this class: its accepting nodes. A name with no Node object in the cluster is skipped. Required, at least one entry. |
| nodes.&lt;name&gt;.interfaces | list of string | Interface names that node's DNAT rules match on. One rule per interface. Naming them per node lets one class cover nodes whose NICs are named differently. Required, at least one. |
| servingMode | enum | `Single` or `Multi`: how many accepting nodes program a mapping. Default `Single`. See [serving modes](datapath.md#serving-modes). |
| ports.min, ports.max | int | The range a PortMap may ask for. Defaults 1, 65535. |
| ports.reserved | list of int | Ports the admin keeps back. A PortMap naming one is rejected in status. |
| namespaceSelector | label selector | Which namespaces may reference this class. Empty selects every namespace. |
| returnPath.mode | enum | `Vxlan` or `None`. `None` refuses any mapping that would need a return link. |
| returnPath.vxlan.vni, port, subnet, subnet6 | | Deprecated and ignored. The schema keeps them for one release so an existing manifest still validates, and a class that sets one reports `Deprecated=True` naming it. Leave the vxlan block out. |

A class sets no link numbers. Each link gets its VNI, addresses, packet mark,
routing table and device name from its own [Link](#link), and its UDP port from
the agent's [--link-port](#agent-flags). No two Links share a slot, so two
classes whose accepting nodes overlap build their links on different numbers
with nothing set on either class.

## PortMap

Namespaced. The workload writes it, beside its Service.

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: PortMap
metadata:
  name: gameserver
  namespace: games
spec:
  className: public
  protocol: UDP
  port: 3000
  serviceRef:
    name: gameserver
    port: game
```

| Field | Type | Meaning |
| --- | --- | --- |
| className | string | The PortMapClass this asks for. Required, immutable. |
| protocol | enum | TCP or UDP. Required, immutable. |
| port | int | The port clients dial on an accepting node. With endPort set, the first port of the range. Required, immutable. |
| endPort | int | Last port of an inclusive range starting at port. Omit for a single port. Immutable together with port; the API rejects endPort below port. One nftables `dport <first>-<last>` match covers a whole range. |
| interfaces | list of string | The interfaces this mapping binds on, narrowing what the class gives each node. Empty selects every interface the class gives the node. Mutable. |
| serviceRef.name | string | A Service in the same namespace. Required. |
| serviceRef.port | string | The named port on that Service. Its number in the EndpointSlice, the pod's containerPort, is the port the pod receives on. Required. |

The Service may be any type, ClusterIP included. It exists so the agent has
endpoints to follow and a named port to resolve. kuport never touches it, and
reads neither its ClusterIP nor its `port` and `targetPort` numbers.

### Port translation

The agent sends the request to the chosen pod on the number the EndpointSlice
lists under `serviceRef.port`. For a Service whose `http` port has
`targetPort: http` and a pod with `containerPort: 8096` named `http`, this
mapping answers on 10002 and delivers to 8096:

```yaml
spec:
  className: public
  protocol: TCP
  port: 10002
  serviceRef:
    name: jellyfin
    port: http
```

The accepting node's ruleset then carries:

```
meta nfproto ipv4 iifname "enp1s0" tcp dport 10002 counter dnat ip to 10.244.17.52:8096
oifname "cilium_host" ip daddr 10.244.17.52 tcp dport 8096 counter snat ip to ip saddr
```

When the pod sits on another node, the request crosses the return link on the
port the client dialed, and the pod's node writes the new port as it writes the
pod's address. The reply rules on the pod's node match the pod's port.
[datapath.md](datapath.md) has the rules on each node.

A mapping whose number is the same as `port` renders exactly as it did before
translation existed. Two cases are refused with Accepted=False:

| Case | Reason |
| --- | --- |
| a range (`endPort` set) whose named port lists a number other than `port`; a range delivers each port as dialed | RangeTranslation |
| a second mapping on the same Service port and protocol when either of the two translates; the pod's node matches replies by pod address and port, and two mappings would write two return rules for the same replies | TargetPortInUse, naming the earlier mapping |

The immutable fields are immutable because changing them is indistinguishable
from deleting one mapping and creating another, and the reconcile is simpler if
it never has to unwind a half-changed mapping.

### IPv6

The Service decides which families a mapping is delivered in; the PortMap has
no field for it. The agent chooses the pod from the Service's IPv4
EndpointSlices, then looks the same pod up in its IPv6 EndpointSlice, matching
the endpoint's targetRef.

| Service `ipFamilyPolicy` | EndpointSlices | Delivered over |
| --- | --- | --- |
| SingleStack, IPv4 | IPv4 | IPv4, with the rules every earlier release wrote |
| PreferDualStack or RequireDualStack, on a dual-stack cluster | IPv4 and IPv6 | IPv4 and IPv6, both to the same pod |
| SingleStack, IPv6 | IPv6 | nothing: the mapping reports `Programmed=False` with `NoReadyEndpoint` |

An IPv6 endpoint counts only when it is the chosen pod and ready. Port
translation applies to both families in the same way. A node delivers IPv6 only
while its host forwards IPv6; [datapath.md](datapath.md#ipv6) has the rules each
node writes and that requirement.

An interface that holds no IPv6 address still gets the IPv6 DNAT rule, because
the rule matches the interface and the port. No IPv6 packet arrives there, and
its published row carries no `address6`.

## Choosing interfaces

A mapping's interfaces resolve against each accepting node's own list, so one
list covers nodes whose NICs are named differently. Given a class that gives
worker-1 wt0 and enp1s0, and worker-2 wt0 and eth0:

| The mapping asks for | worker-1 binds | worker-2 binds |
| --- | --- | --- |
| nothing | wt0, enp1s0 | wt0, eth0 |
| wt0, enp1s0, eth0 | wt0, enp1s0 | wt0, eth0 |
| wt0 | wt0 | wt0 |
| enp1s0 | enp1s0 | nothing |

Asking for wt0 alone keeps a mapping on the overlay. Asking for the LAN
interfaces as well as wt0 reaches clients on both.

An interface name that goes nowhere is reported, in one of three places
depending on where the mistake is:

| Mistake | Reported as | Where |
| --- | --- | --- |
| The mapping names an interface no node in the class carries | Accepted=False, InterfaceNotInClass | the PortMap |
| The mapping names interfaces the class carries, none of them on the node that ended up serving | Programmed=False, NoInterfaceOnNode | the PortMap |
| The class gives a node an interface the host does not have | the node row is not ready, `interface eth0 not present` | the PortMapClass, and as Programmed=False, NodeNotReady on each mapping it serves |

A name some node carries is accepted, since binding on a subset of the nodes is
the point of the field. The second row catches the case that gets through: a
mapping admitted by the class check whose serving node has none of the names it
asked for, which would otherwise program nothing in silence.

## PortMap status

```yaml
status:
  endpoint:
    node: worker-b
    address: 10.244.18.107
    address6: fd00:10:244:12::6b
  published:
    - node: edge-a
      interface: enp1s0
      address: 203.0.113.9
      address6: 2001:db8::9
    - node: edge-a
      interface: wt0
      address: 100.64.93.143
  observedGeneration: 3
  conditions:
    - type: Accepted
      status: "True"
      reason: Valid
    - type: Programmed
      status: "True"
      reason: AllNodesReady
```

`published` is what a person needs: the addresses this port answers on right
now, one row per class interface on the mapping's serving node. It is what
`kubectl get portmap` prints in its wide columns.

The serving node writes the mapping's status, and every published row names one
of its own interfaces, so it resolves each address off its own host. A row whose
interface has not resolved yet carries an empty address and fills in on the next
pass.

`endpoint.address6` is the pod's IPv6 address when the serving node delivers the
mapping over IPv6. Only then does each row carry `address6`, the interface's
first global IPv6 address, which `kubectl get portmap -o wide` prints in the
PUBLISHED6 column. In the example the wt0 row has none, because wt0 holds no
IPv6 address on edge-a.

| Condition | True when | Notable false reasons |
| --- | --- | --- |
| Accepted | the class exists, selects this namespace, the port range is inside the class range and unreserved, and no earlier mapping holds it | `ClassNotFound`, `NamespaceNotSelected`, `PortOutOfRange`, `PortReserved`, `PortConflict`, `InvalidPortRange`, `RangeTranslation`, `TargetPortInUse` |
| Programmed | the serving node has written its rules and its class status row reports ready, and for a pod on another node the return link has landed with usable addresses at both ends | `NoReadyEndpoint`, `ReturnPathUnavailable`, `NodeNotReady` |

`InvalidPortRange` is the ceiling on the CEL rule that endPort must not be below
port: an update the API server let through still reports here.

`Programmed` gates on the participants: the serving node's row in the class
status, and for a remote pod the landed link slot. Under Single the other
accepting nodes hold nothing for the mapping, so their rows gate nothing. Until
a gate clears, the status names what is missing, and the level-driven reconcile
heals it.

## Class status

The class's accepting nodes write its status, the `nodes` rows and the
conditions. A node that only holds a mapping's pod writes nothing to the class.

```yaml
status:
  nodes:
    - name: edge-a
      ready: true
      underlayMTU: 1400
      linkMTU: 1350
      addresses:
        enp1s0: 203.0.113.9
        wt0: 100.64.93.143
      addresses6:
        enp1s0: 2001:db8::9
    - name: worker-c
      ready: false
      message: interface wt0 not present
  observedGeneration: 2
  conditions:
    - type: Deprecated
      status: "False"
      reason: NoDeprecatedFields
    - type: Ready
      status: "True"
      reason: Valid
```

Releases up to v0.7.1 also wrote `status.links` here, one entry per node pair
with a return link. The class's accepting agents remove the field on their first
pass, and the claims live in [Link](#link) objects.

One `nodes` row per accepting node, written by that node's agent, carrying the
MTU numbers and interface addresses it read from the host. The row is ready when
every interface the class gives that node resolves to an address there;
otherwise it carries a message naming the first interface that does not. The
class names each node's interfaces, so an interface that fails to resolve is a
mistake in the class rather than a node that lacks the NIC, and the row says so
rather than skipping it.

`addresses6` holds the first global IPv6 address of each of those interfaces
that has one, while the node forwards IPv6. On a node whose
`net.ipv6.conf.all.forwarding` is 0, `addresses6` stays empty, and if one of the
interfaces holds an IPv6 address, the row carries `ipv6Unavailable` with the
reason. Neither field changes `ready`: IPv4 works either way.

`linkMTU` is that node's own figure, `underlayMTU` less the encapsulation. A
link device takes the smaller of its two ends' figures, so a link to a peer on a
thinner underlay carries less than either row suggests. [MTU in
troubleshooting.md](troubleshooting.md#mtu) has the reading.

| Condition | True when | Notable false reasons |
| --- | --- | --- |
| Ready | the link pool has a slot for every pair the class needs, and the agent's link port differs from the CNI's tunnel port | `VxlanPortConflict`, `SubnetExhausted` |
| Deprecated | the class sets a field kuport ignores. The reason is `DeprecatedField`, and the message names each field as `returnPath.vxlan.<field>` | `NoDeprecatedFields`, the healthy state |

Deprecated leaves Ready alone: a class that sets `returnPath.vxlan.vni` still
works, and the field has no effect.

`SubnetExhausted` carries the message `all 1024 slots of the cluster-wide link
pool are taken`. `TunnelModeRequired` was another false reason until v0.4.0,
when both legs moved onto kuport's own return link and the CNI's routing mode
stopped deciding anything. `InvalidSubnet` went with the per-class subnets after
v0.7.1.

## EgressClass

Cluster-scoped, short name `egc`. The admin writes it. It names the egress
node: the node whose address the selected pods' outbound connections leave the
cluster from.

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: EgressClass
metadata:
  name: edge-a
spec:
  node: edge-a
  namespaceSelector:
    matchLabels:
      kuport.wlz.li/egress: allowed
```

| Field | Type | Meaning |
| --- | --- | --- |
| node | string | The egress node. Required. Mutable: changing it moves every Egress of the class to the new node on the next pass. |
| namespaceSelector | label selector | Which namespaces may reference this class. Empty selects every namespace. |

An EgressClass sets no link numbers, interfaces or ports. Its links come from
the [Link](#link) pool like a PortMapClass's, and the egress node's main
routing table picks the interface the traffic leaves by.

### EgressClass status

```yaml
status:
  nodes:
    - name: edge-a
      ready: true
      underlayMTU: 1400
      linkMTU: 1350
    - name: worker-b
      ready: true
      underlayMTU: 1350
      linkMTU: 1300
  observedGeneration: 1
  conditions:
    - type: Ready
      status: "True"
      reason: Valid
```

Each node taking part writes its own `nodes` row: the egress node, and every
node holding a pod one of the class's Egresses selects. The row carries the MTU
figures the link is sized from, and `ipv6Unavailable` when the node does not
forward IPv6. A pod's path waits for both rows, the egress node's and its own
node's.

| Condition | True when | Notable false reasons |
| --- | --- | --- |
| Ready | the link pool has a slot for every pair the class needs | `SubnetExhausted` |

`kubectl get egressclass` prints NAME, NODE and AGE.

## Egress

Namespaced, short name `eg`. The workload writes it. It selects pods in its own
namespace whose connections toward the default route leave the cluster from
the class's egress node.

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: Egress
metadata:
  name: qbittorrent
  namespace: media
spec:
  className: edge-a
  podSelector:
    matchLabels:
      app.kubernetes.io/name: qbittorrent
  failClosed: true
```

| Field | Type | Meaning |
| --- | --- | --- |
| className | string | The EgressClass this asks for. Required, immutable. |
| podSelector | label selector | Pods in the Egress's namespace. Required. The API refuses a selector with neither matchLabels nor matchExpressions, which would select every pod in the namespace. |
| failClosed | bool | True drops a selected pod's traffic toward the default route while its path through the egress node is down. False lets that traffic leave from the pod's own node meanwhile. Default false. Mutable. |

Only traffic that would take the pod node's default route changes path. A
destination with a more specific route on the pod's node (another pod, a node
address, the LAN, a mesh peer) keeps its path with or without an Egress, and
whatever `failClosed` says. [datapath.md](datapath.md#egress) has the rules.

An Egress and a PortMap are independent. A pod can leave through the egress node
with no inbound port, and a pod with both gets two links to that node, one per
class.

A pod two Egresses select belongs to the earlier one, by creationTimestamp and
then by namespace and name. The later Egress leaves the pod out of its status
and reports `EgressConflict`.

### Egress status

The egress node's agent writes the status. When the class names no Node, the
first node of the cluster by name writes it.

```yaml
status:
  node: edge-a
  pods:
    - name: qbittorrent-0
      node: worker-b
      address: 10.244.18.107
      path: Link
      address6: fd00:10:244:12::6b
      path6: Link
  observedGeneration: 1
  conditions:
    - type: Accepted
      status: "True"
      reason: Valid
    - type: Programmed
      status: "True"
      reason: AllPodsRouted
```

`pods` lists every selected pod that is scheduled, holds an address and has not
finished. `path` says how its IPv4 traffic toward the default route leaves, and
`path6` its IPv6 traffic:

| Path | Meaning |
| --- | --- |
| Link | over a link to the egress node, which masquerades it to its own address |
| Local | the pod runs on the egress node and leaves through the CNI's masquerade there |
| Blocked | the path is down and the Egress fails closed: the traffic is dropped |
| Direct | the path is down and the Egress does not fail closed: the traffic leaves from the pod's own node |

A path is Link once the pair's Link exists and the EgressClass has rows from both
the egress node and the pod's node. `path6` is Link only when the link also
carries IPv6 and both nodes forward it.

| Condition | True when | False reasons |
| --- | --- | --- |
| Accepted | the class exists and selects this namespace | `ClassNotFound`, `NamespaceNotSelected` |
| Programmed | every selected pod's path is Link or Local; reason `AllPodsRouted` | `EgressNodeNotFound`, `EgressConflict`, `PodCIDRsUnknown`, `SubnetExhausted`, `ReturnPathUnavailable` |

When several false reasons apply, Programmed reports the first in that order.
[troubleshooting.md](troubleshooting.md#egress) gives the meaning and the fix
for each.

`kubectl get egress` prints NAME, CLASS, NODE (from `status.node`), FAIL-CLOSED
and AGE.

## Link

Cluster-scoped. The agents write it; a person reads it. Each Link holds one slot
of the cluster-wide pool for one return link between two nodes.

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: Link
metadata:
  name: slot-1
spec:
  classKind: PortMapClass
  className: public
  peers: [edge-a, worker-b]
status: {}
```

| Field | Type | Meaning |
| --- | --- | --- |
| metadata.name | string | `slot-<n>`, n from 0 to 1023. The name is the slot, so the API server refuses a second Link on the same slot. |
| spec.classKind | enum | `PortMapClass` or `EgressClass`. Immutable, like the whole spec. |
| spec.className | string | The class the link serves. Two classes linking the same two nodes get a Link each. |
| spec.peers | list of two strings | The two node names, sorted. |
| status.unusedSince | time | Set when no mapping or Egress needs the link, cleared when one needs it again. Absent while the link is in use. |

### Numbers a slot fixes

| Number | Slot n | slot-1 between edge-a and worker-b |
| --- | --- | --- |
| VXLAN VNI | 7000 + n | 7001 |
| IPv4 ends | the nth /31 of 169.254.64.0/21 | edge-a 169.254.64.2, worker-b 169.254.64.3 |
| IPv6 ends | the nth /127 of fd64:f5ac:e961::1:0/112 | edge-a fd64:f5ac:e961::1:2, worker-b fd64:f5ac:e961::1:3 |
| packet mark | 0x6b700000, n's low 8 bits in bits 0-7, its next 2 bits in bits 12-13 | 0x6b700001 (slot 300 gives 0x6b70102c) |
| routing table | 10000 + n | 10001 |
| device name | `kup-<n>` | kup-1 |
| UDP port | the agent's `--link-port`, the same for every link | 4790 |

The alphabetically first node holds the low address of each pair, in both
families. A link whose MTU is below 1280 carries IPv4 alone.

### Who writes a Link

| Write | Agent | When |
| --- | --- | --- |
| create `slot-<n>` | the agent holding a mapping's pod, or a pod an Egress selects, on that pair | a needed pair has no Link. It picks the lowest slot no Link in its cache holds, and an AlreadyExists answer sends it to the next pass |
| set `status.unusedSince` | any agent | no mapping or Egress needs the pair |
| clear `status.unusedSince` | any agent | a mapping or an Egress needs the pair again |
| delete | any agent | `unusedSince` is more than 24 hours old |
| delete | the agent holding the pod | two Links name the same class and peers; every agent uses the lower slot, and this agent deletes the other |

Every status write and delete carries the resourceVersion the agent read, so a
write against a Link another agent has changed is dropped and decided again on
the next pass. [architecture.md](architecture.md#status-writing) has the
sequence, and [decisions.md](decisions.md#link-address-allocation) the reasons
for a cluster-wide pool.

`kubectl get links` prints one row per Link:

| Column | From |
| --- | --- |
| NAME | `slot-<n>` |
| KIND | spec.classKind |
| CLASS | spec.className |
| PEERS | spec.peers |
| UNUSED-SINCE | status.unusedSince, as an age; empty while a mapping or an Egress needs the link |

## Agent flags

Each flag falls back to an environment variable, which is how the DaemonSet
sets them.

| Flag | Variable | Default | Meaning |
| --- | --- | --- | --- |
| --node-name | NODE_NAME | none | this node's name; the DaemonSet sets it from spec.nodeName |
| --log-level | LOG_LEVEL | info | debug, info, warn or error |
| --resync | RESYNC | 5m | informer resync period |
| --health-addr | HEALTH_ADDR | :8081 | address of /healthz and /readyz |
| --link-port | LINK_PORT | 4790 | UDP port of every return link |

`--link-port` needs the same value on every node, because each end of a link
sends to the port the other end listens on. It must differ from the CNI's tunnel
port, 8472 for Cilium; when the two match, every Vxlan class reports
`Ready=False` with `VxlanPortConflict`. It replaces the deprecated class field
`returnPath.vxlan.port`.
