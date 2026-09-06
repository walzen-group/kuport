# Integrating kuport into a cluster

Written to the agent doing the integration. It assumes you have the cluster's
repo in front of you and nothing else: no conversation, no notes. You are
installing kuport, a DaemonSet that programs nftables and routes so a TCP or
UDP port on one node reaches a pod on another node with the client's source
address intact. Read docs/spec.md for why it is shaped this way; this page is
the procedure.

One fact before any step: kuport was built without access to a cluster. Its
unit, golden and envtest suites pass locally under the project's pinned
toolchain and run again in CI on every push; the end-to-end script the spec
describes has not been written, because no cluster was available to write it
against. Your cluster will be among the first. Verify each step below against
the observable it names rather than trusting the sequence, and when something
refuses to match, docs/operations.md is written for exactly that moment.

## Preconditions

Check all four before installing anything. Each is load-bearing: kuport makes
a missing one visible as a condition on the class, and nothing more. It will
not degrade around it and it cannot fix it.

### Step 1: Cilium in tunnel mode

```sh
kubectl -n kube-system get cm cilium-config \
  -o jsonpath='{.data.routing-mode}'
```

Expected result: `tunnel`. Older Cilium names the same fact with a `tunnel`
key set to `vxlan` or `geneve`; either one is a go. `native` is the hard stop.
When the config map is absent or carries neither key, kuport cannot establish
the mode and refuses nothing on that guess, so this manual check is the only
check: verify the encapsulation yourself before continuing.

Native routing breaks the inbound leg outright. The client address survives
the hop between nodes only because Cilium's vxlan encapsulation carries the
packet between the two nodes' own addresses, which is what WireGuard's source
check accepts. With native routing there is no encapsulation to hide behind:
the forwarded packet carries the client's source into a mesh that discards
it. The agent reads this configuration and reports `Ready=False` with reason
`TunnelModeRequired` on each Vxlan-mode class; the mapping rules underneath
still program, so status on such a cluster reads green per mapping and the
cross-node traffic dies inside the mesh anyway. A None-mode class never
checks the tunnel, and a cluster without cilium-config is reported as unknown
rather than refused. If the answer is `native`, stop here or move the cluster
to tunnel mode. This is not a warning.

### Step 2: pod addresses routable between nodes

The DNAT target is always a pod address, so a pod on one node must be able to
reach a pod on another.

```sh
kubectl run kuport-mtu-probe --rm -it --image=nicolaka/netshoot -- \
  sh -c 'hostname -i'
```

Note the address, then from a pod on a different node:

```sh
ping -c 2 <that pod address>
```

Expected result: replies. On a tunnel-mode Cilium this is the default; a
failure means a NetworkPolicy or a broken CNI, and kuport inherits the
problem, so fix it first.

### Step 3: the mesh admits node-to-node UDP on your link port

kuport builds point-to-point VXLAN links between node pairs on one UDP port.
It must differ from the CNI's, which is 8472 for Cilium. The default is 4790.

```sh
# on node A
nc -lu 4790
# on node B
echo kuport-probe | nc -u <A's mesh address> 4790
```

Expected result: the line appears on A. The outer packets of a return link
carry the two nodes' mesh addresses, so this is the same admission any
node-to-node traffic needs; it fails only when the mesh filters UDP. Pick
another port in that case and carry it into the class as
`returnPath.vxlan.port`.

### Step 4: MTU headroom

The return link adds 50 bytes of encapsulation to traffic that the CNI has
already budgeted. Do the arithmetic on the real cluster, not the spec's
numbers:

| Quantity | How to read it | Example |
| --- | --- | --- |
| mesh MTU | `ip link show` on the WireGuard interface, per node | 1400 |
| CNI configured MTU | `cilium status` MTU line, or the CNI config map | 1400 |
| real pod-to-pod MTU | from a pod: `ping -M do -s 1372 <pod on another node>` and step down until it answers | 1350 |
| kuport link budget | real pod-to-pod minus 50 | 1300 |

Expected result: a number greater than the payloads your workload replies
with. Zero margin is workable (the design cluster runs at exactly zero),
negative is not: replies over the link silently disappear, and the symptom
looks like packet loss, never like a kuport error. After install,
`kubectl get portmapclass <name> -o jsonpath='{.status.nodes}'` shows each
node's own underlay and link numbers.

## Installing

Apply the CRDs before either path, from the release's `crds-<version>.yaml`.
Helm installs its `crds/` directory once and never upgrades it, so the bundle
is the upgrade path for the schemas either way.

The two install paths and the CRD bundle come from a release on the project's
GitHub Releases page; the chart is also pushed to the registry as an OCI
artifact.

| Artifact | What it is |
| --- | --- |
| `kuport-<version>.yaml` | the rendered deploy tree: namespace, ServiceAccount, ClusterRole, DaemonSet, with the image already pinned to the release's digest |
| `kuport-<version>.tgz` | the Helm chart, same contents, parameterized |
| `crds-<version>.yaml` | both CustomResourceDefinitions on their own version track |

The plain manifests are the default. Choose the chart when the `classes`
value (which creates PortMapClass objects with the release) is easier than a
second apply, or when your tooling installs charts anyway.

### Pod security

The rendered manifests label their Namespace object
`pod-security.kubernetes.io/enforce: privileged`. The agent needs it, because
hostNetwork and NET_ADMIN sit outside the baseline standard. The chart creates
no namespace of its own, so a chart install labels the namespace before
installing; the chart's README carries that step.

On a cluster enforcing baseline, a namespace without the label takes the
DaemonSet and rejects every pod it asks for. It reads as DESIRED with a CURRENT
of 0, and the DaemonSet's own status says nothing about why. The reason is in
its events:

```sh
kubectl -n kuport-system describe daemonset kuport-agent
```

Expected result on a healthy install: no FailedCreate events. A line reading
`violates PodSecurity "baseline:latest"` is the missing label.

```sh
kubectl apply -f crds-<version>.yaml
# plain path:
kubectl apply -f kuport-<version>.yaml
# or the chart path:
helm install kuport kuport-<version>.tgz
# or the chart straight from the registry:
helm install kuport oci://ghcr.io/walzen-group/kuport --version <version>
```

A chart from a release carries the image digest in its packaged values. The
chart in a source checkout defaults its image tag to `v<appVersion>`, a tag
that only exists once the release has run: installing it before that needs an
explicit tag or digest.

Whichever you take, record the image reference in the form
`ghcr.io/walzen-group/kuport-agent@sha256:<digest>`. Pin by tag and digest
together; the digest decides what runs, the tag is for people. The release
notes carry the digest, and `kuport-<version>.yaml` has it baked in already.

If your cluster's config is declarative (Flux, Terragrunt output, anything),
commit the digest-pinned reference and let the pipeline apply it. kuport does
not care how the YAML arrives, and nothing in it is cluster-specific.

### Step: confirm the DaemonSet is up

```sh
kubectl -n kuport-system get pods -l app.kubernetes.io/name=kuport
```

Expected result: one pod per node, Running. The agent needs no cluster-wide
traffic; it only talks to the API server and the host kernel.

## Writing the PortMapClass

This is the part that needs thinking. There is no template that fits every
cluster, because the class records the cluster's shape: which nodes have the
addresses, which interfaces carry them, who may ask. Work the two examples
below against `kubectl get nodes -o wide`, `ip addr` on those nodes, and the
topology in your head, then write your own.

The reference cluster has one edge node with a public address and workers
behind home connections, joined by a WireGuard mesh named `wt0`. Its two
classes:

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: PortMapClass
metadata:
  name: public
spec:
  nodeSelector:
    matchLabels:
      kuport.wlz.li/edge: "true"
  interfaces:
    - enp1s0
    - wt0
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

Why this shape. The nodeSelector names the nodes that hold the addresses
clients dial: usually one. Both `enp1s0` (the public interface) and `wt0`
(the overlay address) go into `interfaces`, so the same mapping answers on
the LAN address and the overlay address at once, which is what makes one
PortMap serve both players on the internet and players on the VPN. The
Vxlan return path is required because the pod will not live on the edge node.

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: PortMapClass
metadata:
  name: intranet
spec:
  nodeSelector:
    matchExpressions:
      - key: node-role.kubernetes.io/control-plane
        operator: DoesNotExist
  interfaces:
    - wt0
  returnPath:
    mode: None
```

Why this shape. Every worker accepts, so a workload with a pod on each worker
has a ready candidate on an accepting node, the global endpoint choice lands
on one of those, and that node serves the mapping from a local pod: the
same-node path, with no return machinery to build. The port answers on the
chosen worker's `wt0` address, and the address to dial is the one in the
mapping's `published`. The class refuses any mapping whose chosen pod sits on
a node the class does not select, which for this selector means the control
plane. A workload that may schedule there needs a Vxlan class.

Which fields are decisions and which are defaults:

| Field | Decision or default | Notes |
| --- | --- | --- |
| nodeSelector | decision | must be written; it selects the accepting nodes |
| interfaces | decision | must be written; names that exist on every selected node, or the missing ones simply produce no rules there |
| ports | default 1..65535 | worth narrowing on a public class |
| ports.reserved | default none | the admin keeps ports back; nothing else respects them |
| namespaceSelector | policy decision | empty means every namespace may ask; on a public class that is rarely what you want |
| returnPath.mode | decision | required; Vxlan or None |
| returnPath.vxlan.vni | default 4242 | anything that does not collide with other vxlan use on the mesh |
| returnPath.vxlan.port | default 4790 | must differ from the CNI's 8472 |
| returnPath.vxlan.subnet | default 169.254.77.0/24 | /31 per node pair, 128 slots; link-local space avoids route table surgery |

Labels the selectors match do not exist until you create them:

```sh
kubectl label node <edge-node> kuport.wlz.li/edge=true
kubectl label namespace <some-ns> kuport.wlz.li/public=allowed
```

### Step: confirm the class is Ready

```sh
kubectl get portmapclass public -o jsonpath='{.status.conditions}'
```

Expected result: `Ready=True, reason Valid`. A false condition's reason is
documented row by row in docs/operations.md. The `nodes[]` rows fill in as
each agent reports; an empty list after a minute means no selected node is
running an agent.

## Letting a workload ask

Three pieces, all in the workload's namespace. The namespace label (above). A
Service exposing a named port. And the PortMap beside it:

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
  endPort: 3009
  serviceRef:
    name: gameserver
    port: game
```

`endPort` opens the inclusive range 3000-3009 as one mapping; omit it for one
port. The Service may be any type, ClusterIP included. It exists so the agent
has endpoints to follow and a named port to resolve, and kuport never
modifies it. The port name (`game`) is what `serviceRef.port` matches.

## Verifying the install

In order, each with the observable that proves it. Do not skip to the last
one: the earlier steps localize the fault, and the last one proves a thing
none of the others can.

1. DaemonSet has a pod on every node whose class selection you expect:
   `kubectl -n kuport-system get pods -o wide`.
2. Class reports `Ready=True` and its `nodes[]` rows are populated:
   `kubectl get portmapclass <name> -o yaml`.
3. PortMap reports `Accepted=True, reason Valid`, then
   `Programmed=True, reason AllNodesReady`:
   `kubectl -n <ns> describe portmap <name>`.
4. `published` lists the addresses clients should dial:
   `kubectl -n <ns> get portmap <name> -o wide`.
5. A probe from outside the cluster shows the client's own address arriving
   at the pod. With an echo responder: send a UDP line from a host whose
   address you know, and the pod answers it back as the peer. Or `tcpdump
   -i any port <port>` at the pod's node: the packets you capture carry your
   source address.

A quirk in step 4 to expect, not a fault: the rows name the serving node and
its class interfaces from the first pass, while an address can stay blank
until the serving node's agent resolves that interface on its host. The names
are complete from the start; only the address can lag a pass.

Step 5 is the only one that proves the datapath. Steps 1-4 describe what the
agents wrote and reported; they can all be green while the port stays shut,
most often because of a host firewall in `filter`, which kuport neither writes
nor inspects. If 1-4 pass and 5 times out: docs/operations.md, the host
firewall section and the counters section, in that order.

## What kuport will not do

So you do not go looking for it:

- HTTP routing or TLS. Use an ingress controller.
- Load balancing one port across pods on several nodes. One endpoint is
  chosen per mapping, deterministically, and reprogrammed when it moves.
- IPv6. Every rule is in the `ip` family.
- Admission webhooks. Conflicts are reported in status, never blocked at
  apply time.
- Anything in the `filter` table. The host firewall is yours.
- Serving one mapping from several nodes at once. Exactly one accepting node
  serves each mapping, chosen from the shared inputs, so the port answers on
  one node's addresses; the other accepting nodes hold no rules for it.
- Surviving a rollout with connections intact. Established connections
  through a node drop while its agent restarts, and the port refuses (rather
  than blackholing) while an endpoint has no ready pod.

## Upgrades

The agent version is semver and moves independently of the CRD version. A new
release may change one and not the other.

The chart deliberately never upgrades CRDs, because a naive Helm upgrade that
dropped the CRD would delete every PortMap in the cluster. When a release's
notes say the schema changed (the group version is `v1alpha1` until it is
not), apply the new bundle by hand first:

```sh
kubectl apply -f crds-<new-version>.yaml
```

Then upgrade the agent through whichever path you installed by. The order
matters only when a schema change adds a field the agent writes: new CRDs,
then new agent. Deleting the agent's CRDs while objects exist deletes every
PortMap, so no tool should ever do it for you.
