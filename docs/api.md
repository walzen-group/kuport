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

Two classes whose accepting nodes overlap need their own `vni` and `subnet`. A
class numbers its slots from zero, so two classes sharing a node would both ask
for the base VNI and the first /31 there, and the kernel refuses the second of
each.

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
| port | int | The port clients dial on an accepting node, and the port the pod receives on. With endPort set, the first port of the range. Required, immutable. |
| endPort | int | Last port of an inclusive range starting at port. Omit for a single port. Immutable together with port; the API rejects endPort below port. One nftables `dport <first>-<last>` match covers a whole range. |
| interfaces | list of string | The interfaces this mapping binds on, narrowing what the class gives each node. Empty selects every interface the class gives the node. Mutable. |
| serviceRef.name | string | A Service in the same namespace. Required. |
| serviceRef.port | string | The named port on that Service. Required. |

The Service may be any type, ClusterIP included. It exists so the agent has
endpoints to follow and a named port to resolve. kuport never touches it.

The immutable fields are immutable because changing them is indistinguishable
from deleting one mapping and creating another, and the reconcile is simpler if
it never has to unwind a half-changed mapping.

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
  published:
    - node: edge-a
      interface: enp1s0
      address: 203.0.113.9
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

| Condition | True when | Notable false reasons |
| --- | --- | --- |
| Accepted | the class exists, selects this namespace, the port range is inside the class range and unreserved, and no earlier mapping holds it | `ClassNotFound`, `NamespaceNotSelected`, `PortOutOfRange`, `PortReserved`, `PortConflict`, `InvalidPortRange` |
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

`linkMTU` is that node's own figure, `underlayMTU` less the encapsulation. A
link device takes the smaller of its two ends' figures, so a link to a peer on a
thinner underlay carries less than either row suggests. [MTU in
troubleshooting.md](troubleshooting.md#mtu) has the reading.

| Condition | True when | Notable false reasons |
| --- | --- | --- |
| Ready | the subnet parses, has a free slot, and the link port does not collide with the CNI's | `VxlanPortConflict`, `SubnetExhausted`, `InvalidSubnet` |

`TunnelModeRequired` was a fourth reason until v0.4.0, when both legs moved onto
kuport's own return link and the CNI's routing mode stopped deciding anything.
