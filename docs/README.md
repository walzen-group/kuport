# kuport documentation

kuport delivers a TCP or UDP port from one node's addresses to a pod, keeping
the client's source address, and lets the workload ask for that itself.

Start with [overview.md](overview.md) if you have not met it before.

## By what you are doing

| You want to | Read |
| --- | --- |
| understand what this is and whether it fits your cluster | [overview.md](overview.md) |
| write a PortMapClass or a PortMap | [api.md](api.md) |
| know why a packet goes where it goes | [datapath.md](datapath.md) |
| find out why a mapping is not working | [troubleshooting.md](troubleshooting.md) |
| install it into a cluster | [integration.md](integration.md) |
| know why the design is shaped this way | [decisions.md](decisions.md) |
| understand how the agents divide the work | [architecture.md](architecture.md) |
| build, test or release the agent | [development.md](development.md) |

## By document

**[overview.md](overview.md)** states the problem, the goals and non-goals, and
what a cluster has to provide.

**[api.md](api.md)** covers both kinds field by field, how a mapping's
interfaces resolve against each node's, and what the two status blocks report.

**[architecture.md](architecture.md)** is the DaemonSet with no central
controller: how every agent computes the same answer, which one writes a given
status, and how link slots are claimed through the API server.

**[datapath.md](datapath.md)** is the packet's path and every rule on it, the
same-node and cross-node shapes, the serving modes with an example class for
each, the return link, and MTU.

**[decisions.md](decisions.md)** records the closed questions with the
measurement that settled each one, including the four approaches to cross-node
`Multi` that were closed before the one that works.

**[troubleshooting.md](troubleshooting.md)** is for a mapping that is not
working: reading status, the counters, the failures that leave no log line, the
host firewall, MTU, and what happens to host state when the agent moves.

**[integration.md](integration.md)** brings kuport into a cluster.

**[development.md](development.md)** is the repo layout, the implementation
notes, the test tiers and the release pipeline.

## History

Two documents were folded into these and removed. `spec.md` became
[overview.md](overview.md), [api.md](api.md),
[architecture.md](architecture.md), [datapath.md](datapath.md),
[decisions.md](decisions.md) and [development.md](development.md).
`spec-decoupled-inbound.md` proposed carrying the request on kuport's own link;
it was implemented in v0.3.6 and v0.4.0, so its design lives in
[datapath.md](datapath.md) and its reasoning in
[decisions.md](decisions.md#carrying-both-legs-on-kuports-own-link).
