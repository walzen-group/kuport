# API

Group `kuport.wlz.li`, version `v1alpha1`. Two kinds: a class the admin writes
and a mapping the workload writes.

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
    vxlan:
      vni: 4242
      port: 4790
      subnet: 169.254.77.0/24
      subnet6: fd64:f5ac:e961::/112
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
| returnPath.vxlan.vni | int | Base VXLAN network identifier for the links this class builds. Each slot takes the base plus its own number, because the kernel keys a device by VNI and port. |
| returnPath.vxlan.port | int | UDP port for the links. Must differ from the CNI's, which is 8472 for Cilium. Default 4790. |
| returnPath.vxlan.subnet | CIDR | Link addresses are allocated from here, a /31 per node pair (RFC 3021 point-to-point), so the default gives 128 slots. The slot for a pair is a recorded claim in status.links rather than a computed hash. Default 169.254.77.0/24. |
| returnPath.vxlan.subnet6 | IPv6 CIDR | IPv6 link addresses are allocated from here, a /127 per node pair (RFC 6164) at the pair's slot in `subnet`. The API refuses a prefix that is not IPv6, and one holding fewer /127s than `subnet` holds /31s. Default fd64:f5ac:e961::/112, a ULA prefix whose 40 random bits follow RFC 4193. |

Two classes whose accepting nodes overlap need their own `vni` and `subnet`. A
class numbers its slots from zero, so two classes sharing a node would both ask
for the base VNI and the first /31 there, and the kernel refuses the second of
each. When both classes deliver IPv6, give them their own `subnet6` too, or the
first /127 collides the same way.

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

The class reports each selected node's readiness verdict and which return links
hold an address slot.

```yaml
status:
  links:
    - key: edge-a/worker-b
      peers: [edge-a, worker-b]
      subnet: 169.254.77.2/31
      slot: 1
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
    - type: Ready
      status: "True"
      reason: Valid
```

One `links` entry per node pair that has, or recently had, a return link. The
`unusedSince` field appears when no PortMap needs the pair and is set to the
time that became true; an entry unused for 24h is dropped. The slot fixes the
routing table (`200 + slot`), the packet mark (`0x6b700000 | slot`) and the
link's VNI, so a claim is never renumbered while a link uses it.

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
| Ready | the subnet parses, has a free slot, subnet6 is a usable IPv6 prefix, and the link port does not collide with the CNI's | `VxlanPortConflict`, `SubnetExhausted`, `InvalidSubnet` |

`TunnelModeRequired` was a fourth reason until v0.4.0, when both legs moved onto
kuport's own return link and the CNI's routing mode stopped deciding anything.
