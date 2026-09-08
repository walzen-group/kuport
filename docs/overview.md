# Overview

kuport delivers a TCP or UDP port from one node's addresses to a pod, keeping
the client's source address, and lets the workload ask for that itself.

Status: v0.4.0, 2026-09-08. Both serving modes carry traffic on the test
cluster, verified across every entry point in UDP and TCP with the client's
address intact on each reply, cross-node included. Every rule in
[the datapath](datapath.md) carried real traffic by hand before it was written
down, and each one names the measurement that confirmed it.

## The problem

A Kubernetes cluster spread across sites has one node with a public address and
the rest behind home connections, joined by a WireGuard mesh. A game server, a
mail daemon, or anything else without an HTTP header to carry the truth needs
the client's real address. Nothing in Kubernetes does this.

| Mechanism | Why it does not apply |
| --- | --- |
| cloud load balancer | the provider owns the path; there is no provider |
| MetalLB, L2 or BGP | wants the entry point and the nodes on a routable network |
| Cilium DSR | the target replies directly with the service address as source, which home NAT rewrites |
| externalTrafficPolicy Local | requires the pod on the node the packet arrived at |
| hostPort | publishes only on the node the pod runs on |
| a reverse proxy or tunnel | re-originates the connection; the pod sees the proxy |

The gap is narrow and real: entry node and workload node in different NAT'd
sites, joined by a mesh whose tunnel enforces which source addresses each peer
may send.

## Goals

- A workload declares a port in its own namespace, beside its Service.
- The mapping follows the pod. A reschedule reprograms rather than drops.
- One mapping answers on several interfaces of the accepting node at once, so
  the same port is reachable on a LAN address and an overlay address.
- An internal port works the same as a public one; only the class differs.
- The pod sees the client's real address on every path.
- Two workloads asking for one port is reported rather than silently dropped.
- An admin decides which nodes, interfaces and ports exist, and who may ask.

## Non-goals

- HTTP routing or TLS. An ingress controller already does that better.
- Load balancing one port across pods on several nodes. One endpoint is chosen
  and reprogrammed when it goes away. `servingMode: Multi` spreads the accepting
  side rather than the serving side: every accepting node programs the mapping,
  so a routed address survives one of them going away, and all of them forward
  to that same one endpoint. See [serving modes](datapath.md#serving-modes).
- IPv6 in the first version. Every rule is `table ip`.
- Admission webhooks. Conflicts are reported in status; the reasoning is under
  [decisions](decisions.md).
- Replacing the CNI. kuport writes rules beside it.

## Requirements of the cluster

| Requirement | Why |
| --- | --- |
| the agent can write nftables and routes on the host | it runs as a DaemonSet with hostNetwork and NET_ADMIN |
| nodes reach each other on an address the mesh accepts | both legs of a remote mapping ride a link whose outer header uses those addresses |
| the CNI gives a pod an address on its own node | the last translation hands the packet to the pod through the CNI's own path there |

Two requirements were dropped in v0.4.0. Until then the inbound leg travelled
the CNI's tunnel, which meant Cilium had to be in tunnel mode and pod addresses
had to be routable between nodes. Both legs now ride kuport's own return link,
so neither holds and native routing no longer breaks anything. The agent's
`TunnelModeRequired` condition went with them, and
[the decision](decisions.md#carrying-both-legs-on-kuports-own-link) records why.

What remains is generic: a CNI that gives a pod an address on its own node, and
nodes that can reach each other. Supporting other CNIs is separate work and no
claim is made here that it is done.

## Where to go next

| Document | For |
| --- | --- |
| [api.md](api.md) | the two kinds, their fields, and what status reports |
| [architecture.md](architecture.md) | how the agents divide the work between them |
| [datapath.md](datapath.md) | the packet's path and every rule on it |
| [decisions.md](decisions.md) | why the design is shaped this way |
| [troubleshooting.md](troubleshooting.md) | a mapping is not working |
| [integration.md](integration.md) | bringing kuport into a cluster |
| [development.md](development.md) | building, testing and releasing it |
