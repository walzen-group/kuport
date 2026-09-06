# kuport operations

For the person holding a mapping that is not working. Each section names the
command to run and the answer that tells you what to do next.

One honest note up front: kuport shipped from unit, golden and envtest tests
and nothing else. There was no test cluster while it was written, and the
hand-run end-to-end script the spec describes has not been written yet. Where
this page describes a failure mode, it comes from the design measurements in
docs/spec.md or from the conditions the code reports, not from an incident
log. A cluster outage is still the place where new failure modes will be
found.

## Reading status

The first move is always the same:

```sh
kubectl get portmap -n <namespace> <name> -o wide
```

Expected result: rows with CLASS, PROTO, PORT, ENDPOINT and, with `-o wide`,
PUBLISHED. `published` lists the addresses the port answers on right now, one row per
accepting node and interface. That is the thing to hand to whoever calls in
from outside. Each row's address comes from the accepting node's own report
into the class status, so with several accepting nodes a row can carry a blank
address until that node's agent has reported its row. The node and interface
names are always complete.

What the shapes tell you:

| What you see | What it means |
| --- | --- |
| A row with a populated `published` | rules exist on every accepting node for those addresses |
| `Accepted=True`, `Programmed=False` | the mapping is admissible but at least one node has not programmed it; read the reason |
| `Accepted=False` | the mapping will never be programmed until the reason is fixed; every reason is a decision the API made about the object |
| An empty status, nothing in `published`, no conditions | no agent has taken the object. Something is wrong below the mapping: check the DaemonSet |

The last row deserves a sentence of its own: an absent condition is
information. Every agent that can see the API computes a status owner for
every PortMap (the endpoint holder, else the first accepting node, else the
first node), so a PortMap with no status at all means no agent pod is running,
not that the reconcile skipped it.

The class has its own report:

```sh
kubectl get portmapclass <name> -o yaml
```

Read `status.nodes` (one row per accepting node: ready, message, the MTU
numbers and the interface addresses it resolved) and `status.links` (one entry
per node pair that holds a return-link address slot, with its key, subnet and
slot).

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
| AllNodesReady | Programmed | True | every accepting node has written its rules for this mapping | if the port still fails traffic, the fault is outside the rules: see the host firewall and counters |
| NoReadyEndpoint | Programmed | False | the named port of the referenced Service has no ready endpoint; the DNAT rule is removed, so the port refuses (reset, ICMP unreachable) instead of blackholing | `kubectl -n <ns> get endpointslice -k kubernetes.io/service-name=<svc>`; readiness of the pods |
| ReturnPathUnavailable | Programmed | False | the pod is on another node and the return link cannot be built: class mode is None, a node has no usable address, or the class subnet has no slot | the class `returnPath`, its Ready condition, and whether the node objects carry addresses |
| NodeNotReady | Programmed | False | an accepting node's agent has not reported ready, so its half is missing | the DaemonSet pod on that node, the node itself, and the class `nodes[]` row's message |
| RemotePodMultipleAcceptingNodes | Programmed | False | the class selects several accepting nodes and the pod is on none of them; only the first accepting node by name programs it, this node reports and emits nothing | a design limit, see spec; use one accepting node for remote pods, or a mode None class with a local DaemonSet pod |

### PortMapClass conditions

| Reason | Status | Meaning | First check |
| --- | --- | --- | --- |
| Valid | True | the subnet parses and has free slots; tunnel mode and port checks are overlaid by the agents from the host | nothing, this is the healthy state |
| TunnelModeRequired | False | the agent read the CNI's configuration and it is not tunneling (for Cilium: `routing-mode` is not `tunnel`), so the inbound leg cannot preserve the client address | `kubectl -n kube-system get cm cilium-config -o jsonpath='{.data.routing-mode}'` |
| VxlanPortConflict | False | the class link port equals the CNI's VXLAN port (8472 on Cilium); the links would swallow the CNI's own traffic | change `returnPath.vxlan.port` |
| SubnetExhausted | False | every /31 slot in `returnPath.vxlan.subnet` is claimed; a new pair cannot be allocated | widen the subnet (a /24 gives 128 slots); claims of pairs unused for 24h are dropped by the GC on their own |
| InvalidSubnet | False | `returnPath.vxlan.subnet` does not parse as a CIDR | fix the field |

## The counters

Every rule kuport writes carries a `counter`, so the datapath answers the
question "did the packet get here" without logs. Run it on the node, in the
host's namespace (the agent is hostNetwork, so anything that can exec on the
node works):

```sh
nft list table ip kuport
```

You will see the three chains and, on each rule, `counter packets N bytes M`.
What a climb means, and what a zero means, stage by stage:

| Rule | Counter climbing | Counter stuck at zero |
| --- | --- | --- |
| `kup-pre` DNAT (accepting node) | traffic arrives at that interface and port | nothing reaches the node on that tuple: wrong address dialed, the host firewall (see below), or upstream routing. Compare `published` against where the client actually sends |
| `kup-post` with `oifname "cilium_host"` (same-node pod) | the inbound leg is being claimed before Cilium's masquerade; the pod should see the client address | with the DNAT counter climbing, the pod is on another node; look at the target node instead |
| `kup-mangle` mark rule (target node) | replies from the pod are marked and policy-routed back to the accepting node | the pod's replies do not match src and sport: it replies from another address or port, or it never received the request |
| `kup-post` with `oifname != "cilium_*"` (target node) | the reply leaves through the return link toward the accepting node | replies are taking some other route: check the ip rule ladder and the link, next sections |

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
ip -s link show dev kup-<hash>
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
main) and pref 102 (lookup the per-slot table). One line per link is the
loop.

## The host firewall

kuport writes `nat` and `mangle` chains in its own table. It writes nothing in
`filter`, and it does not look for a foreign filter chain either. That is a
decision, recorded in docs/spec.md: a workload-authored PortMap must not be
able to punch a hole in the host firewall.

The consequence is the most likely first-install surprise: a node running
firewalld, ufw, or any policy that drops forwarded or input traffic will drop
the client's packet before kuport's rules matter, and kuport will report the
mapping fully healthy. `Accepted=True`, `Programmed=True`, `published`
populated, DNAT counters at zero. Nothing contradicts anything, because status
describes the rules kuport owns, not the machine's other rules.

Open the port with whatever manages the firewall on that node. That is the
TCP/UDP port clients dial, on each interface named in the class, plus nothing
between nodes: the return link's packets carry the nodes' own addresses and
ride the mesh like any node-to-node traffic. If clients time out with clean
status and zero DNAT counters, look here first.

## MTU

The class status carries the arithmetic:

```sh
kubectl get portmapclass <name> -o jsonpath='{.status.nodes}'
```

`underlayMTU` is the MTU of the interface carrying the return links, read from
that node. `linkMTU` is that number minus 50, the VXLAN overhead. A reply
larger than `linkMTU` cannot cross a return link.

A margin of zero is not an error. On the cluster this was designed against,
the mesh carries 1350, Cilium is configured at 1350, and a real pod-to-pod
probe crosses at 1300; the link budget of 1350 minus 50 fits with nothing
spare. Check your own numbers rather than assuming, because the failure mode
is quiet: oversized datagrams vanish.

There is a second trap the pod never sees coming. Cilium sets a pod's
interface MTU to the underlay MTU, not 50 below it, so a pod is always told 50
more than it can actually send between nodes. TCP absorbs this through
packetization-layer path MTU discovery in blackhole mode. UDP does not. A
workload sending large datagrams, and a game server is exactly that workload,
must keep its own send size under the real figure: `linkMTU` from the class
status is the safe ceiling.

## Agent lifecycle

What happens to the host state when the agent moves:

| Event | Effect |
| --- | --- |
| Clean shutdown (SIGTERM, rolling update, node drain) | the agent removes its nftables table, its kup- links, its routing rules and routes. A node taken out of a class stops holding state nobody wants |
| Crash (OOM, node power loss) | the rules stay. The next start reconciles: it rewrites the table whole and removes owned objects the new desired state does not name |
| Rolling update of the DaemonSet | `maxUnavailable: 1`, one node at a time. On each node there is a window of seconds where the table is gone and rebuilt: the port refuses during the window, and established connections through that node are dropped, because flushing the table drops their NAT bindings |
| Endpoint pod reschedules | the DNAT target changes on the next pass of every accepting node. Old connections do not carry over; the port refuses for the gap and serves again once the new endpoint is ready |

The rollout interruptions are the accepted cost of the NoReadyEndpoint
decision: refuse fast, never blackhole. If your workload cannot tolerate a
few refused seconds per node per release, that is the number to design around,
not kuport's bug.
