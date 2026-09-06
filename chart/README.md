# kuport

A Helm chart for the kuport agent: the DaemonSet that delivers a node's TCP or
UDP port to a pod on another node, keeping the client's source address. The
plain manifests under `deploy/` are the primary install path: every
release attaches them with the image pinned by digest. This chart exists for
clusters where templating the class objects and the image reference is easier
than patching YAML by hand. Where this chart and `deploy/` disagree,
`deploy/` is right.

## Requirements

- Kubernetes with Cilium in tunnel mode and a WireGuard-style mesh, as
  described in the [spec](../docs/spec.md). kuport is not portable to any
  cluster.
- A cluster-admin for the first install: the chart registers two CRDs.

## Install

1. Install the release into its own namespace:

   ```
   helm install kuport ./chart -n kuport-system --create-namespace
   ```

   Expected result: `STATUS: deployed`.

2. Check the rollout:

   ```
   kubectl -n kuport-system get daemonset kuport-agent
   ```

   Expected result: `DESIRED` and `READY` equal the node count. The agent
   tolerates every taint on purpose, so a missing pod means the node is
   unreachable, not tainted.

3. Write your first PortMapClass. The `classes` value creates class objects
   with the release, so one values file covers the install and the classes:

   ```
   helm upgrade kuport ./chart -n kuport-system --set-json \
     'classes={"public":{"nodeSelector":{"matchLabels":{"kuport.wlz.li/edge":"true"}},"interfaces":["enp1s0"],"ports":{"min":1024,"max":65535,"reserved":[80,443]},"namespaceSelector":{"matchLabels":{"kuport.wlz.li/public":"allowed"}},"returnPath":{"mode":"Vxlan"}}}'
   ```

   Expected result: `STATUS: deployed` and
   `kubectl get portmapclass public` lists the class.

A plain `helm install ./chart` from a git checkout pulls the default tag,
`v<appVersion>`. That tag exists only after the matching release has been
cut. Pin `image.tag` or `image.digest` until then.

Each `classes` entry is a class name mapped to a PortMapClass spec, written
out as its rendered manifest verbatim. A field added to the API later needs no
chart change.

## Image reference

The helper resolves the image in this order: `image.digest`, then `image.tag`,
then `v<appVersion>`. Release images are published under a tag with the v
prefix (`v1.2.3`), so the fallback names a tag a release publishes.
Release artifacts default `image.digest` to the pushed digest, so the pin
travels with the chart version and the tag stays for people to read. The
packaged chart is also pushed to `oci://ghcr.io/walzen-group/kuport`, so a
Flux-style consumer can pull it by version with that digest intact.

## CRDs

The CRDs live in `crds/`, not in `templates/`. Helm installs that directory
before the templates and never upgrades or deletes it. That is the right
behaviour for a CRD: an upgrade that dropped the CRD would delete every
PortMap in the cluster, release or not.

It also means Helm will not update the schemas for you. When a kuport release
changes a CRD schema, apply the new one by hand before or after the upgrade:

```
kubectl apply -f chart/crds/
```

`helm uninstall` removes the release's objects, including any PortMapClass you
created through `classes`, and leaves the CRDs and everything stored under
them in place. Delete the CRDs yourself only if you mean to erase every
PortMap in the cluster:

```
kubectl delete -f chart/crds/
```

## Values

| Key | Default | Meaning |
| --- | --- | --- |
| image.repository | ghcr.io/walzen-group/kuport-agent | Where the agent image is pulled from |
| image.tag | "" (v<appVersion>) | Image tag when no digest is set |
| image.digest | "" | `sha256:...` pin; when set it wins over the tag |
| image.pullPolicy | IfNotPresent | Pull policy for the agent container |
| nameOverride / fullnameOverride | "" | Name parts used by the object names |
| rbac.create | true | Create the ClusterRole and binding |
| serviceAccount.create | true | Create the ServiceAccount |
| serviceAccount.name | "" (fullname-agent) | Use this ServiceAccount name, created or pre-existing |
| logLevel | info | Agent log level, passed as LOG_LEVEL |
| resources | 10m/32Mi requests, 128Mi memory limit | CPU limit is deliberately absent; see values.yaml |
| tolerations | tolerate all | Taints the agent pod survives |
| nodeSelector | {} | Extra node labels for running the agent |
| classes | {} | PortMapClass objects to create with the release |
