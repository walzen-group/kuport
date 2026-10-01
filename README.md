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

kuport has run on the maintainers' test and production clusters since
September 2026. Both run Talos nodes spread across sites, joined by a netbird
WireGuard mesh, with Cilium in tunnel mode. In production, PortMaps deliver TCP
to Jellyfin and InfluxDB. The test cluster serves UDP to an Insurgency Sandstorm game server and
runs the echo fixtures that cover both serving modes over TCP and UDP.

GitHub Actions runs the unit, golden and envtest suites on every push. The
end-to-end tier runs by hand against the test cluster, as
[docs/development.md](docs/development.md) describes. IPv6 delivery is new in
v0.6.0 (released 2026-10-01), and no PortMap on either cluster uses it yet.

Every cluster kuport has run on shares that one design. On a cluster built
differently, install on a staging cluster first and walk
[docs/integration.md](docs/integration.md) end to end.

[docs/README.md](docs/README.md) indexes all of it. The ones most people want:

- [docs/overview.md](docs/overview.md): the problem, the goals, and what a
  cluster has to provide.
- [docs/api.md](docs/api.md): both kinds field by field, and what status
  reports.
- [docs/datapath.md](docs/datapath.md): the packet's path, every rule on it
  with the measurement that confirmed it, and the serving modes.
- [docs/troubleshooting.md](docs/troubleshooting.md): a mapping is not working,
  and what every condition reason means.
- [docs/integration.md](docs/integration.md): installing kuport into a specific
  cluster, written for whoever does the installing.

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
each dependency and the reason kuport needs it.
