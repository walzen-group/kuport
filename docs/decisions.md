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
one mode. What it costs is Hubble's view of the request leg, which had already
lost the reply leg when the return path was built. The conntrack cost that was
quoted against it turned out to be nothing: both nodes already held an entry per
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

A client learns immediately instead of hanging until its own timeout. The cost is
accepted on both sides: connections do not survive a rollout, and the old rule is
never left in place to keep them alive.

## Link address allocation

A recorded claim in `PortMapClass.status.links` rather than a computed hash. A
/31 per node pair out of the class subnet, 128 slots in the default /24. The
endpoint-holding agent allocates the lowest free slot and writes the claim; the
accepting agent reads it. The API server's optimistic concurrency is the arbiter:
a 409 means re-read, redo, retry.

The API server is already a consistent store with compare-and-swap, so this needs
no leader, no election, and no agent-to-agent protocol. Collisions become
impossible rather than handled, the allocation is visible in
`kubectl get portmapclass -o yaml`, and the scheme reuses the single-writer rule
the design already had for status. A computed hash was rejected because two
agents computing the same slot from stale views cannot detect each other, which
is a collision-repair protocol in disguise.

## Link release

A claim is freed after 24h unused, and any agent the class selects may run the
garbage collection. The link device is not held by that window: it is torn down
immediately by being absent from the next desired state. The status entry lingers
only to hold the address reservation.

Nothing carries traffic through a free window, so the GC needs no locking, and
clock skew between nodes is irrelevant against 24h. A per-pair teardown protocol
was rejected for the same reason the allocation uses the API server: the device
lifecycle is already level-driven, and the reservation is the only thing that
needs a timer.

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
