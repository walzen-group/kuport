# Decoupling the inbound leg from the CNI

Status: design, 2026-09-08. Not implemented. Three of the measurements below
were taken on the test cluster; the one that decides the design is outstanding
and is named at the end.

kuport carries the reply between nodes on its own vxlan link and borrows the
CNI's tunnel for the request. This document proposes carrying the request on
the same link. That is a small change to where one DNAT points, and it removes
two of the project's four cluster requirements, makes `servingMode: Multi`
work as specified, and closes the dead end recorded in Findings below.

## What is asymmetric today

```
 request:  client -> A -> DNAT to the pod -> Cilium's tunnel -> pod
 reply:    pod -> T's host stack -> kuport's link -> A -> client
```

The reply already bypasses the CNI. Only the request rides it, and only
because the DNAT target is a pod address on another node, which is what makes
ordinary routing hand the packet to the CNI.

Two of the four requirements in spec.md exist solely to serve that one leg.

| Requirement | Why it exists |
| --- | --- |
| Cilium in tunnel mode | its encapsulation carries the client's source past WireGuard's check |
| pod addresses routable between nodes | the DNAT target is a pod address on another node |

## What changes

One translation, on the accepting node.

```
today    on A:  dnat to <pod>:<port>          destination is a pod, routing picks the CNI
proposed on A:  dnat to <peer link addr>:<port>   destination is on the /31 attached to kup-<T>,
                                                  routing picks the link
```

Nothing intercepts the packet and no policy routing is added. The destination
decides the path, so changing the destination changes the path.

The peer then receives a packet addressed to itself, which is delivered locally
and therefore traverses netfilter, and translates it the rest of the way.

```
 client                accepting node A                    pod's node T
    |  dst <A's address or a VIP>:<port>
    +--- wt0 --->  kup-pre: dnat to 169.254.79.3:<port>
                     src still CLIENT
                        |
                        +==== kup-<T> ====>  kup-mangle:
                                               iifname "kup-<A>"
                                                 ct mark set <peer mark>
                                                    |
                                             kup-pre: dnat to <pod>:<port>
                                                    |
                                             delivered through cilium_host
                                             (the CNI's own path, unchanged)
                                                    |
                                               pod sees src = CLIENT
                                                    |
                                               pod replies
                                                    |
                                             kup-mangle:
                                               meta mark set ct mark
                                             pref 101/102 -> table -> kup-<A>
                        <==== kup-<A> ====+
                   conntrack reverses both hops
    <--- wt0 ---+
```

Both translations are reversed by conntrack on the way back, and the client's
address is never rewritten: the destination is translated inbound and the
source outbound, so the pod sees the real client exactly as it does today.

## Why this makes Multi work

`servingMode: Multi` fails today because the pod's node cannot learn which
accepting node forwarded a request, and the reply carries nothing that says
which. Under this design the request arrives on that node's own link, so the
interface is the identity. The existing conntrack save and restore rules then
work as written, because the packet is addressed to the node and reaches
netfilter rather than being delivered by the CNI.

This is the general case, not a virtual-address special case. Any accepting
node can forward, and a reply finds its way back to whichever one did. A
routed virtual address is then one way of reaching the accepting set rather
than a mode of its own.

The same-node case is unaffected. A programmer that holds the pod translates
straight to it and builds no link, exactly as today.

## What the requirements become

| Requirement | After |
| --- | --- |
| Cilium in tunnel mode | drops. kuport's own encapsulation carries the client's source |
| pod addresses routable between nodes | drops. Each node reaches only its own pods |
| the agent can write nftables and routes | unchanged |
| nodes reach each other on a mesh-accepted address | unchanged, and now carries both directions |

What remains is generic: a CNI that gives a pod an address on its own node,
and nodes that can reach each other. The `TunnelModeRequired` condition and
the native-routing check in integration.md exist because kuport depends on an
encapsulation it does not control, and both become unnecessary rather than
merely easier to satisfy.

This document does not propose supporting other CNIs. It records that the
dependency is removed; whatever per-CNI parameters and priorities that would
need is separate work.

## What this does not change

The pod is still reached through the CNI's own path on its node, so endpoint
policy applies to ingress and egress as it does today. Node-to-node traffic is
still inside the mesh's WireGuard, so it is still encrypted. The reachable set
does not widen: the peer translates to exactly the pod the PortMap names.

## Costs

**Link MTU becomes load-carrying.** The links are created at MTU 1500 while
the path underneath them is smaller, 1350 on the test cluster. That is
harmless today because the links carry only small replies. Carrying requests
puts real payloads on them, and the symptom of getting this wrong looks like
packet loss rather than a kuport error. The agent already reads the underlay
MTU and reports `linkMTU`; it has to set the device's MTU from it as well.

**Source identity on ingress.** The CNI derives a security identity for a
packet entering a pod. Arriving over kuport's link rather than the CNI's
tunnel, it resolves from the source address instead. The source is the
client's address in both cases, so the identity is expected to be unchanged,
and that expectation is worth confirming before a default-deny policy relies
on it.

**Observability.** Flow tools that observe the CNI's datapath see neither
direction of a kuport mapping once the request moves. They already see neither
for the reply.

**A second translation.** Two NAT hops per flow instead of one, each reversed
by conntrack, so each node holds an entry for the flow. Both age on the same
timeouts recorded under servingMode in spec.md.

## Findings that led here

Each closed one approach to making Multi work, measured on the test cluster.

| Approach | Measurement |
| --- | --- |
| Conntrack keyed on the arrival interface | the restore rule counted 6 packets while both save rules stayed at 0: the request never reaches netfilter on the pod's node |
| A tag carried in the packet, DSCP | the accepting node stamped 12 packets, the pod's node counted 0 in prerouting: same bypass |
| The pod's node replying directly with the virtual address as source | the mesh dropped it; a control with the node's own source arrived in the same second |
| Routing a pod-addressed request over the link | the pod received nothing; a control on the normal path forked a socat child in the same second |

The fourth is what this design changes. A pod-addressed packet arriving on a
foreign device is not delivered, because delivery to a pod belongs to the CNI
and it accepts only its own devices. A packet addressed to the node itself is
delivered locally, and the translation to the pod then happens on the node
that owns it.

## The measurement still outstanding

One question decides whether this design is buildable, and it is the same
shape as the four above.

Put a packet on a link addressed to the peer's own link address, translate it
on the peer to a local pod, and observe whether the pod receives it. The
expectation is that it does, because it is the ordinary host-to-pod path that
a kubelet health check already uses. That expectation is exactly the kind that
the findings above falsified four times, so it is measured before anything is
written.

Two smaller ones follow it: whether the CNI masquerades traffic leaving a node
on a `kup` device, which would rewrite the client's source, and whether the
link MTU change is sufficient for a full-size payload.
