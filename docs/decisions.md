# Decisions

Each of these is closed. They are recorded so they are not relitigated, with the
reasoning that settled them and, where one exists, the measurement.

## Carrying both legs on kuport's own link

The accepting node translates a remote pod to the far end of its own return
link, so the request rides kuport's encapsulation rather than the CNI's tunnel.
Applied to `Multi` in v0.3.6 and to both modes in v0.4.0.

`Multi` forced it. The pod's node has to send each reply back to whichever node
forwarded the request, and nothing in a reply says which. The identity has to
come from the request, and the request has to reach netfilter on that node for
anything to record it.

Four approaches were closed by measurement first, in that order:

| Approach | Measurement that closed it |
| --- | --- |
| Conntrack keyed on the arrival interface, with the request still on the CNI's tunnel | the restore rule counted 6 packets while both save rules stayed at 0: the request never reached netfilter on the pod's node |
| A tag carried in the packet, DSCP | the accepting node stamped 12 packets, the pod's node counted 0 in prerouting: the same bypass |
| The pod's node replying directly with the virtual address as source | the mesh dropped it; a control with the node's own source arrived in the same second |
| Routing a pod-addressed request over the link | the pod received nothing; a control on the normal path forked a responder in the same second |

The fourth is what the design changes. A pod-addressed packet arriving on a
foreign device is not delivered, because delivery to a pod belongs to the CNI and
it accepts only its own devices. A packet addressed to the node's own end of the
link is delivered locally, traverses netfilter, and the translation to the pod
then happens on the node that owns it.

**Single moved onto it too, in v0.4.0.** It had no such need, since one node
forwards and the identity is never ambiguous. It moved because the alternative
was maintaining two datapaths for the rest of the project's life, and because
doing so drops both of kuport's CNI requirements rather than dropping them for
one mode. Hubble no longer sees the request leg; it had already lost the reply
leg when the return path was built. The conntrack cost that was quoted against
it turned out to be nothing: both nodes already held an entry per
flow, and measurement showed the pod's node holding an `[UNREPLIED]` record of
the reply. The change makes that entry a proper established flow instead.

## IPv6 inside the IPv4 link

IPv6 delivery reuses each return link: the /127 sits on the same kup- device
as the /31, and the outer header stays IPv4 between the nodes' InternalIP
addresses. Decided 2026-10-01.

A second set of links with an IPv6 underlay would need every node pair to
reach each other over IPv6 through the mesh, and a second device, VNI and slot
per pair. The IPv4 path between nodes already exists for every IPv4 mapping,
and VXLAN carries Ethernet frames, so an IPv6 packet crosses it unchanged. The
pair keeps one slot, one mark and one routing table number in both families.

The rules live in one `table inet kuport`. Two tables, ip and ip6, would
duplicate the chains and leave the agent writing two transactions per pass, and
a failure between them would program one family and not the other. The inet
table rewrites both families in the single transaction the ip table used.

The Service's EndpointSlices decide the families, so the PortMap API gained no
field. The pod is still chosen from the IPv4 slices, which keeps the choice
every agent makes identical to the one it made before IPv6, and the IPv6 slice
supplies only that pod's second address.

## Multi keeps one globally chosen endpoint

A node-local endpoint choice was proposed, where each accepting node serves a pod
on itself. It makes a workload with a pod on every accepting node work with no
datapath change, and it was rejected: the target case is one Postgres primary
behind a routed address, which a node-local choice does not serve.
`chooseEndpoint` stays a single shared computation.

## No virtual-address mode

When it looked as though only direct server return could work, the feature was
going to become a mode with the routed address declared on the class. That is
unnecessary, because the identity comes from the arrival interface rather than
from a declared address. A routed virtual address is one way of reaching the
accepting set rather than a mode of its own.

## rp_filter stays as it is

Loosening it on the pod's node was proposed while the design still routed a
pod-addressed request over the link, and weakening source validation was
rejected. It was then measured at 0 on every device of a worker, so the question
is moot. The design does not depend on it either way: the request is addressed to
the node rather than to a pod.

## Port ranges

`spec.port` is required; `spec.endPort` is optional and inclusive, mirroring
NetworkPolicy's port and endPort. A ranged mapping renders one nftables match,
`dport <first>-<last>`, rather than a rule per port.

A single port needs no new field, a range needs no hundred objects, and the field
pair is one people already know from NetworkPolicy.

## No ready endpoint

The DNAT rule is removed. The port then refuses, with a TCP reset or an ICMP
port-unreachable, rather than blackholing, and the mapping reports
`Programmed=False` with reason `NoReadyEndpoint`.

A client learns immediately instead of hanging until its own timeout. We accept
the consequence on both sides: connections do not survive a rollout, and the old
rule is never left in place to keep them alive.

## Link address allocation

Every return link in the cluster gets its numbers from one pool of 1024 slots,
and an agent claims slot n by creating the cluster-scoped Link object named
`slot-<n>`. Decided 2026-10-11. The slot fixes the link's VNI, its IPv4 and IPv6
addresses, its packet mark, its routing table and its device name;
[api.md](api.md#link) lists the numbers.

Releases up to v0.7.1 kept each class's claims in its own
`PortMapClass.status.links`, and each class numbered its slots from 0. Inside
one class a claim was safe, because one status object held every claim and the
API server's compare-and-swap ordered the writes to it. No check compared the numbers of two
classes. A node holding links of two classes met these collisions:

| Number | What the two classes produced |
| --- | --- |
| mark and table | slot 0 of every class used mark 0x6b700000 and table 200, so the node got two pref 102 rules for one mark and two default routes in table 200 |
| VNI | every class left at the default asked for VNI 4242 on port 4790, and the kernel refuses a second device with the same VNI and port |
| IPv6 link addresses | every class used the default subnet6, fd64:f5ac:e961::/112, so slot 0 of each class put the same /127 on two devices |
| routing table | 200 + slot reached the kernel's default, main and local tables, 253 to 255, at slots 53 to 55 |

The docs told the admin to give overlapping classes their own vni and subnet.
That asked the admin to know which classes overlap, and no condition reported a
class that got it wrong.

A slot computed from a hash of the class and the node pair needs no store. Two
pairs whose hashes land on one slot both program it, and neither agent sees the
other's link in anything it reads. Detecting the collision and moving one pair
would need a repair protocol between the agents.

The Link objects keep the principle the per-class claim had and widen it to the
whole cluster. The API server refuses a second create of `slot-<n>` with
AlreadyExists, so a slot has one owner across every class, with no leader and no
agent-to-agent protocol, and `kubectl get links` lists every allocation. We put
the pool's ranges clear of the old class defaults: VNIs from 7000,
169.254.64.0/21 and fd64:f5ac:e961::1:0/112.

The marks and tables also have to stay clear of what the CNI and the mesh put
on a node. Read off a worker on 2026-10-11:

| Pref | Rule | Owner |
| --- | --- | --- |
| 9 | `fwmark 0x200/0xf00 lookup 2004` | Cilium, its L7 proxy |
| 105 | `lookup main` | netbird |
| 110 | `fwmark 0x1bd00 lookup 7120` | netbird |

A mark of 0x6b700000 or'd with the slot sets bits under 0xf00 from slot 256 on,
and Cilium's rule takes slots 512 to 767 to its proxy table. A table of 1000
plus the slot reaches 2004 at slot 1004. A slot's mark therefore keeps its low
eight bits in the low byte and moves the next two to bits 12 and 13, leaving
0xf00 clear, and its table is 10000 plus the slot.

## Link release

A Link is deleted after 24 hours unused, and any agent may release it. Every
agent computes the needed pairs from the same inputs, so each one sets
`status.unusedSince` on a Link no mapping needs, clears it on a Link a mapping
needs again, and deletes a Link whose `unusedSince` is more than 24 hours old.
An agent on a third node releases a Link whose two nodes have both left the
cluster. The link device is not held by that window: the next desired state
leaves it out, and the agent tears it down. The Link lingers only to hold the
slot.

Each status write and delete carries the resourceVersion the pass read. When
another agent has changed the Link since, the write is dropped, and the next
pass decides again from the newer object.

Releases up to v0.7.1 never released a claim. The agent computed `unusedSince`
and passed it through mergeClaim, which cleared `unusedSince` on every key the
class status already held. A Link's status is written as the pass computed it,
with no merge step in between.

Nothing carries traffic through a free window, so the release needs no locking,
and clock skew between nodes is irrelevant against 24 hours. A per-pair teardown
protocol was rejected for the same reason the allocation uses the API server:
the device lifecycle is already level-driven, and the reservation is the only
state that needs a timer.

## Egress has kinds of its own, with one node per class

Outbound traffic gets its own pair of kinds, EgressClass and Egress, and an
EgressClass names exactly one node. Decided 2026-10-11.

A PortMapClass describes how inbound traffic arrives: its accepting nodes, the
interfaces each one listens on, a port window and a serving mode. None of those
fields means anything for a connection the pod opens, and an outbound path needs
one node. An egress field on PortMap would tie outbound traffic to an inbound
port, so a pod that only opens connections would need a PortMap it never uses.
With separate kinds, a pod can have an Egress and no PortMap, and a pod with
both gets one link per class.

A class with several nodes would leave the agents to choose a node for each pod
and to move pods when that node fails. A flow's public address would then change
during its life, and a tracker or an API that keys on the address would see a
new client. With one node, the admin chooses the address, and moving the
traffic means editing `spec.node`. kuport has no failover to a second node.

## Egress selects pods by label

An Egress selects pods with `podSelector`. A PortMap names a Service, because it
needs two facts only a Service holds: the named port the pod receives on, and
which endpoints are ready.

An Egress needs neither. It carries every connection the pod opens, on any port.
A workload that only opens connections often has no Service, and selecting
through one would make it create a Service just to be selected. kuport reads
only ready endpoints from an EndpointSlice, so a pod failing its readiness probe
would lose its egress path while it still sends traffic. `podSelector` matches
the labels a NetworkPolicy or a CiliumNetworkPolicy already selects by, and the
API refuses an empty selector, which would select every pod in the namespace.

## Egress routes after the other policy rules

On the pod's node, the Egress mark is routed at prefs 32763 to 32765, after
Cilium's and netbird's rules. Pref 32764 looks the destination up in main with
`suppress_prefixlength 0` before pref 32765 looks up the link's table. Decided
2026-10-11 while building, on the admin's requirement that an Egress must not
override a more specific route.

The first build routed the mark at prefs 101 and 102, where a PortMap's return
path sits. At 102 the mark ran ahead of netbird's rules at 105 and 110, and
every marked packet went to the link's table, whose only route is a default into
the link. A selected pod's traffic to netbird peers, to networks netbird routes
and to the LAN then went to the egress node, away from the routes that served it
without an Egress.

At the end of the ladder, any route more specific than main's default answers
first: main at netbird's pref 105 or kuport's 32764, and netbird's table 7120 at
110. Only traffic bound for main's default route reaches the link. One limit
follows from the order: a netbird exit node puts a default route into table
7120, which answers at 110, and a selected pod's traffic then leaves through the
exit node. A PortMap's reply keeps prefs 101 and 102, because its destination
is any client and it must leave by the link whatever main or netbird would
choose; [datapath.md](datapath.md#two-rule-ordering-constraints) has that case.

## failClosed holds traffic in a blackhole table

A blocked pod gets the mark 0x6b70f000, whose rules send its default-route
traffic into table 11024. The table holds only `blackhole default`, in each
family. Decided 2026-10-11 while building.

The first build dropped a blocked pod's packets in nftables, matching every
destination outside the cluster set. That drop also took the pod's traffic to
netbird peers and to the LAN, which an Egress never sends to the egress node,
so a fail-closed pod lost those connections whenever its path went down.

The blackhole sits behind the same suppress rule at 32764 as the routed path.
Failing closed drops exactly the traffic the Egress would have sent to the
egress node, and a destination with a more specific route keeps working. Table
11024 belongs to kuport: each pass removes a blackhole the plan no longer names,
and the agent's teardown empties the table.

## Cluster destinations stay unmarked

kup-egress marks only flows toward an address outside kup-cluster4 and
kup-cluster6, which hold every Node's pod CIDRs and InternalIPs.

Without the sets, the suppress rule would still route a pod-to-pod flow by
main's specific route, so the packet would arrive. The Egress mark would still
replace the marks Cilium sets on in-cluster flows, and Cilium's own rule at pref
9 routes by its mark.

The sets need every Node's `spec.podCIDRs`. A cluster whose IPAM leaves the
field empty, Cilium's cluster-pool mode among them, cannot build them. Every
Egress there reports `PodCIDRsUnknown` and programs no path, and we accept that
requirement to keep in-cluster flows unmarked.

## Fail-closed pods stay blocked through an agent restart

On a clean shutdown, an agent whose node holds a pod of a fail-closed Egress
applies a keep state in place of its teardown: the blocked mark for each such
pod, the blocked rules, the blackhole and the cluster sets. Decided 2026-10-11.

The teardown removes every rule kuport owns. In a rolling update, seconds pass
between the old agent's teardown and the new agent's first pass, and in that gap
the pod's default-route traffic would leave from its own node. That is the leak
`failClosed` exists to stop. With the keep state, the pod stays blocked through
the gap, and the new agent's first pass rewrites the table whole.

Removing kuport while a fail-closed Egress exists leaves the blocked state on
each pod's node until that node reboots, so delete the Egress objects first. A
node with no fail-closed pod tears down as before.

## A vxlan link rather than GRE or a second WireGuard

GRE is lighter on the wire, 24 bytes against 50, and Talos does not ship the
module: `ip link add type gre` answers `Unknown device type`. A second WireGuard
link would work and brings key material, which then needs generating, storing and
renewing.

vxlan has no keys, its module is already loaded because the CNI uses it, and a
link between two nodes came up and passed traffic on the first try.

## Status conditions rather than an admission webhook

A bad PortMap is accepted by the API server and reported in status. A validating
webhook would reject it at submission time, which reads better. It also puts a
webhook in the path of every write to these kinds, with a certificate to keep
valid and a failure policy to choose.

## Host firewall

A mapping answers on a node whose host firewall drops new connections to the
node's own addresses, with no rule per port in that firewall. The class decides
what is exposed: its nodes, the interfaces on each node, the namespaces allowed
to ask, and the port window. A PortMap opens one port inside that window, on
those interfaces, and nothing reaches an interface the class leaves out.

Opening each mapped port in the host firewall puts a second edit beside every
PortMap, in whatever manages the firewall: on Talos, the machine config. The
port is then written twice, and kuport reports the mapping healthy while it
stays shut until someone makes that edit.

Opening the class's port window once in the firewall avoids the second edit, but
a firewall rule matches the port and not the address. It opens every listener
on the node inside the window as well, Cilium's VXLAN port and node-exporter
among them on a Talos node, so the window has to be cut around them.

Translating ahead of the firewall from kup-pre cannot work. The kernel calls
every nat chain from one hook at the dstnat priority, -100, whatever priority
the chain declares, so kup-pre always runs after a filter chain at -110. v0.6.2
moved kup-pre to `dstnat - 20` on the opposite belief, and a trace on a Talos
node showed the packet dropped before kup-pre.

Rewriting the destination before conntrack, at `raw - 10`, gets past the
firewall and breaks a mesh. v0.7.0 did that, and netbird's mark rule at -150,
which marks a packet from wt0 only when its destination is local, no longer saw
the node's address; netbird's forward filter then dropped every mapping on wt0.

kuport rewrites the destination from kup-steer, a filter chain at -120, which
runs at its declared priority: after conntrack has recorded the dialed address
and after a mesh's mark rules, before the firewall. kup-steer replaces a node
address with a stand-in the node does not hold, the firewall sees no node
address and accepts the packet, and kup-pre translates the stand-in. conntrack
writes the dialed address back onto the reply, as it always did.
[datapath.md](datapath.md#host-firewall) walks a packet through it. kup-steer
rewrites addresses and returns no verdict, so a firewall that drops a packet on
other grounds, forwarded traffic say, still drops it.

## Supporting other CNIs is not in scope

v0.4.0 removed the dependency on the CNI's encapsulation, and
[overview.md](overview.md#requirements-of-the-cluster) records what remains.
Building and testing against other CNIs, with their own masquerade rules and
priorities, is separate work nobody has asked for. What is left that is
CNI-specific is the two masquerade exemptions, matched by interface name, which
would need their equivalents identified per CNI.
