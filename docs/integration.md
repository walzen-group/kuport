# Integrating kuport into a cluster

Written to the agent doing the integration. It assumes you have the cluster's
repo in front of you and nothing else: no conversation, no notes. You are
installing kuport, a DaemonSet that programs nftables and routes so a TCP or
UDP port on one node reaches a pod on another node with the client's source
address intact, and, through an Egress, so a pod's outbound connections leave
from one chosen node's address. Read docs/overview.md for why it is shaped this
way; this page is the procedure.

kuport runs in production on its maintainers' clusters, which both use Talos
nodes, Cilium in tunnel mode and a netbird WireGuard mesh. A cluster that
differs from that design can fail a step the maintainers' clusters pass, so
check each step below against the observable it names. When something refuses
to match, docs/troubleshooting.md is written for that moment.

## Preconditions

Check the first four before installing anything, and the fifth when a mapped
Service is dual-stack. Each one matters: kuport makes
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
another port in that case and set it as the LINK_PORT environment variable on
the agent DaemonSet, which the agent reads as `--link-port`. Every node needs
the same port, because each end of a link sends to the port the other end
listens on. The chart has no value for LINK_PORT, so a chart install that needs
another port has to add the variable to the rendered DaemonSet.

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

A link carries IPv6 only when its budget is at least 1280, the IPv6 minimum,
which needs an underlay of 1330 or more.

### Step 5: IPv6 forwarding, for dual-stack Services

Skip this step when no Service behind a PortMap is dual-stack. Otherwise, on
every accepting node and every node a mapped pod may run on:

```sh
cat /proc/sys/net/ipv6/conf/all/forwarding
```

Expected result: `1`. Cilium with IPv6 enabled sets it. A node reading `0`
delivers its mappings over IPv4 only; the agent logs that, and the class
status row of an accepting node says so in `ipv6Unavailable`.

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
| `crds-<version>.yaml` | the CustomResourceDefinitions for PortMapClass, PortMap, EgressClass, Egress and Link, on their own version track |

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

### Who may create a PortMap or an Egress

Both install paths ship two ClusterRoles, kuport-portmap-view and
kuport-portmap-edit, carrying the aggregation labels for the built-in view,
edit and admin roles. They cover PortMaps and Egresses. A subject already bound
to one of those in a namespace can work with both there, the same way it can
with Deployments and Services. Nothing has to be rebound.

The kinds are scoped so that RBAC splits along the line the design already
draws.

| Kind | Scope | Held by |
| --- | --- | --- |
| PortMapClass | cluster | the cluster's administrator: which nodes accept, on which interfaces, which ports, which namespaces |
| PortMap | namespaced | the workload: asks for a port on a class |
| EgressClass | cluster | the cluster's administrator: which node's address outbound traffic leaves from, and which namespaces may ask |
| Egress | namespaced | the workload: asks for its pods' outbound traffic to leave through a class's node |
| Link | cluster | the agents: one return-link slot per object, created and deleted by the agents' own ClusterRole |

portmapclasses, egressclasses and links appear in neither aggregated role,
deliberately. A namespace admin asks for a port or an egress node and cannot
widen the port range, add an accepting node, change the egress node, or admit
its own namespace to a class.

The agent's own ClusterRole reads every Pod in the cluster, because an Egress
selects pods by label and the agents compute each selected pod's link from its
node and addresses.

serviceRef carries a name and a port and no namespace, so a PortMap always
resolves its Service in its own namespace. A mapping in one namespace cannot
be pointed at another namespace's workload.

To grant PortMaps to a subject that is not bound to view, edit or admin, bind
these roles directly:

```sh
kubectl -n <namespace> create rolebinding portmap-author \
  --clusterrole=kuport-portmap-edit \
  --serviceaccount=<namespace>:<name>
```

Expected result: `rolebinding.rbac.authorization.k8s.io/portmap-author created`.
That grants the write verbs; add a second binding to kuport-portmap-view for
the read verbs.

Set rbac.aggregate to false on the chart to keep PortMaps out of the built-in
roles and decide access with roles of your own. The plain manifests always
carry them; delete the two ClusterRoles to opt out there.

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
  nodes:
    worker-1:
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
```

Why this shape. `nodes` names the nodes that hold the addresses clients dial:
usually one. Both `enp1s0` (the public interface) and `wt0` (the overlay
address) go into that node's `interfaces`, so the same mapping answers on the
LAN address and the overlay address at once, which is what makes one PortMap
serve both players on the internet and players on the VPN. A mapping that wants
the overlay alone sets `interfaces: [wt0]` on itself. The Vxlan return path is
required because the pod will not live on the edge node.

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: PortMapClass
metadata:
  name: intranet
spec:
  nodes:
    worker-1:
      interfaces:
        - wt0
    worker-2:
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
| servingMode | default Single | `Multi` puts the rules on every accepting node, which is what a routed virtual address needs. It needs a return link per accepting node without the pod and depends on conntrack; see docs/datapath.md |
| nodes | decision | must be written; every accepting node by name |
| nodes.&lt;name&gt;.interfaces | decision | must be written; the names that exist on that node, so nodes with different NIC names belong to one class |
| ports | default 1..65535 | worth narrowing on a public class |
| ports.reserved | default none | the admin keeps ports back; nothing else respects them |
| namespaceSelector | policy decision | empty means every namespace may ask; on a public class that is rarely what you want |
| returnPath.mode | decision | required; Vxlan or None. Which mappings None refuses, and the reply path that makes the refusal necessary, are in docs/datapath.md under returnPath mode |
| returnPath.vxlan | ignored, leave it out | every link gets its VNI and addresses from a cluster-wide pool of Link objects, and its port from the agent's LINK_PORT. A class that sets vni, port, subnet or subnet6 reports `Deprecated=True`; docs/api.md has the numbers |

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
documented row by row in docs/troubleshooting.md. The `nodes[]` rows fill in as
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
modifies it. The port name (`game`) is what `serviceRef.port` matches. A
single-port mapping delivers to the number the EndpointSlice lists under that
name, so a client can dial one port and reach the pod on another;
[api.md](api.md#port-translation) shows the rules. A range delivers each port
as dialed and needs the pod listening on those numbers.

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
most often because of a host firewall that drops forwarded traffic, which
kuport does not inspect. If 1-4 pass and 5 times out: docs/troubleshooting.md,
the host firewall section and the counters section, in that order.

## Sending a workload's outbound traffic through one node

An Egress makes the connections a workload's pods open toward the internet leave
from one chosen node's address, usually the node with the public address that
the workload's PortMaps publish. Only traffic that would take the pod node's
default route changes path; docs/api.md has the fields and docs/datapath.md the
rules.

### Requirements

- Every Node carries `spec.podCIDRs`. Cilium fills the field with
  `ipam.mode: kubernetes`; its cluster-pool mode leaves it empty, and every
  Egress then reports `PodCIDRsUnknown`:

  ```sh
  kubectl get nodes -o custom-columns=NAME:.metadata.name,PODCIDRS:.spec.podCIDRs
  ```

  Expected result: a CIDR on every row.

- A pod's outbound packets pass through the host's prerouting hook on the
  pod's node, which is where kuport marks them. The kup-egress counter in step 5
  below shows whether they do.

- The workload's network policy allows its egress to the world. kuport changes
  the route the packets take and leaves policy to the CNI, and Cilium drops a
  packet its policy denies before kuport sees it. With Cilium:

  ```yaml
  apiVersion: cilium.io/v2
  kind: CiliumNetworkPolicy
  metadata:
    name: qbittorrent-egress
    namespace: media
  spec:
    endpointSelector:
      matchLabels:
        app.kubernetes.io/name: qbittorrent
    egress:
      - toEntities:
          - world
  ```

### Step 1: write the EgressClass

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: EgressClass
metadata:
  name: edge-a
spec:
  node: edge-a
  namespaceSelector:
    matchLabels:
      kuport.wlz.li/egress: allowed
```

`node` names the node whose address the traffic leaves from. The egress node's
main routing table picks the interface, so the traffic leaves by that node's
default route. Leave `namespaceSelector` out to let every namespace ask.

```sh
kubectl label namespace media kuport.wlz.li/egress=allowed
```

### Step 2: write the Egress beside the workload

```yaml
apiVersion: kuport.wlz.li/v1alpha1
kind: Egress
metadata:
  name: qbittorrent
  namespace: media
spec:
  className: edge-a
  podSelector:
    matchLabels:
      app.kubernetes.io/name: qbittorrent
  failClosed: true
```

Set `failClosed: true` when the workload must never reach the internet from its
own node's address: while the path is down, its default-route traffic is
dropped. With `false` it leaves from its own node until the path is back.
Traffic with a more specific route (the cluster, the LAN, mesh peers) keeps
working in both cases.

### Step 3: confirm the Egress is programmed

```sh
kubectl -n media get egress qbittorrent -o yaml
```

Expected result: `Accepted=True` with reason `Valid`, `Programmed=True` with
reason `AllPodsRouted`, and every pod under `status.pods` with `path: Link`, or
`Local` for a pod that runs on the egress node. A pod with an IPv6 address
carries `path6` as well. A false Programmed reason is explained row by row in
docs/troubleshooting.md, section Egress.

### Step 4: confirm the class rows

```sh
kubectl get egressclass edge-a -o jsonpath='{.status.nodes}'
```

Expected result: a row for the egress node and one for each node holding a
selected pod, each with its `underlayMTU` and `linkMTU`.

### Step 5: probe from the pod

Fetch the pod's public address from an address echo service the pod is allowed
to reach. Expected result: the egress node's public address. The pod's own
node's address means the path is not in use; read the kup-egress counter on the
pod's node as docs/troubleshooting.md, section Egress, describes. A counter
stuck at zero while the pod sends means its packets do not reach the host's
prerouting hook.

A pod that just started may send its first packets from its own node, before
the agent has seen its address. kuport does not close that window.

Delete an Egress with `failClosed: true` before removing kuport. Otherwise its
pods stay blocked on their nodes until each node reboots.

## What kuport will not do

So you do not go looking for it:

- HTTP routing or TLS. Use an ingress controller.
- Load balancing one port across pods on several nodes. One endpoint is
  chosen per mapping, deterministically, and reprogrammed when it moves.
- An IPv6 underlay for the return links, or a Service with IPv6 endpoints
  alone. A dual-stack Service is delivered over both families, with IPv6
  riding inside the IPv4 link; docs/datapath.md has the rules.
- Admission webhooks. Conflicts are reported in status, never blocked at
  apply time.
- Rules in the host firewall. kuport's filter chains rewrite addresses and
  return no verdict, so a firewall that drops forwarded traffic is yours to
  open; one that drops new connections to the node's own addresses needs
  nothing (docs/datapath.md, Host firewall).
- Serving one mapping from several nodes at once. Exactly one accepting node
  serves each mapping, chosen from the shared inputs, so the port answers on
  one node's addresses; the other accepting nodes hold no rules for it.
- Surviving a rollout with connections intact. Established connections
  through a node drop while its agent restarts, and the port refuses (rather
  than blackholing) while an endpoint has no ready pod.
- Egress failover. An EgressClass names one node; moving the traffic means
  editing `spec.node`.
- Egress for a pod's first packets. A new pod sends from its own node until the
  agent has seen its address.
- Per-destination exceptions in an Egress, or selecting its pods through a
  Service. An Egress selects pods by label and changes only default-route
  traffic.

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

### Upgrading from v0.7.1 or earlier

Releases after v0.7.1 number every return link from a cluster-scoped Link
object, which the agent watches from its first start.

1. Apply the new CRD bundle, which adds the Link CRD:

   ```sh
   kubectl apply -f crds-<new-version>.yaml
   ```

   An agent started before the Link CRD exists cannot watch Links and does not
   become ready.

2. Upgrade the agent. On each node, the first pass of the new agent creates a
   Link for each pair it holds a pod for, builds the `kup-<n>` devices, and
   removes the old devices named by a hash, their routes in tables 200 and up,
   and the pref 101 and 102 rules for the old marks. All of them carry kuport's
   names and none is in the new desired state.

   A connection through a cross-node mapping that is open during that pass
   loses its return path once. A mapping whose pod runs on its serving node has
   no link and keeps its connections. Until both nodes of a pair run the new
   agent, the two ends of their link use different VNIs and addresses, so a
   cross-node mapping between an upgraded node and one still on the old agent
   does not answer until the rolling update reaches the second node.

3. Check the pool:

   ```sh
   kubectl get links
   ```

   Expected result: one row per node pair a class needs a link for, named
   `slot-0` upwards.

4. Remove the `vxlan:` block from every class that has one. Its fields are
   ignored. Then read the class conditions:

   ```sh
   kubectl get portmapclass <name> -o jsonpath='{.status.conditions}'
   ```

   Expected result: `Deprecated=False` with reason `NoDeprecatedFields`. A
   `Deprecated=True` condition names the fields still set. The class's accepting
   agents remove the old `status.links` on their first pass.
