# kuport

Delivers a TCP or UDP port from one node's addresses to a pod on another node,
with the client's source address intact, declared by the workload that wants it.

For clusters spread across sites where one node has a public address and the
rest sit behind home connections, joined by a WireGuard mesh. A game server
behind that topology cannot see who is talking to it, and the usual answers do
not apply: a cloud load balancer needs a cloud, MetalLB needs a routable
network, `externalTrafficPolicy: Local` needs the pod on the entry node, and a
proxy re-originates the connection.

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

An admin defines a PortMapClass naming the accepting nodes, their interfaces and
the port range. A workload asks for a port on that class. An agent on each node
programs nftables and routes to make it so, and follows the pod when it moves.

## Status

Built, and not yet run against a cluster. The agent, the API, the CI and the
deploy artifacts are in this repo; the unit, golden and envtest suites pass
locally under the pinned toolchain, and GitHub Actions runs the same tiers on
every push. There was no test cluster while this was
written, so the end-to-end script the spec describes has not been written
yet: yours will be the first run of anything, on a real network. The datapath
rules themselves were proven by hand on a live cluster before the design was
written, and every rule in the spec names the measurement behind it, but that
proof covers the rules, not this implementation of them.

If you are considering it: install on a staging cluster, walk
[docs/integration.md](docs/integration.md) end to end, and expect to file the
first issues.

- [docs/spec.md](docs/spec.md): the design, the datapath with its
  measurements, diagrams, and the closed decisions.
- [docs/operations.md](docs/operations.md): a mapping is not working, and
  every condition reason means something.
- [docs/integration.md](docs/integration.md): installing kuport into a
  specific cluster, written for whoever does the installing.

## Install

Releases carry three artifacts: `crds-<version>.yaml`, the rendered manifests
`kuport-<version>.yaml` with the image pinned by digest, and the chart
`kuport-<version>.tgz`, also pushed as an OCI artifact at
`oci://ghcr.io/walzen-group/kuport`. Apply the CRDs first, then either path.
Pin the image by tag and digest together; the digest decides.
[docs/integration.md](docs/integration.md) walks the full procedure,
including the four cluster preconditions to check before anything installs.

## What it needs

kuport is not portable to any cluster. It needs a CNI in tunnel mode, because
the encapsulation is what carries a client address past WireGuard's source
check, and it needs to write nftables and routes on the host. The spec lists
each dependency and why it is load-carrying.
