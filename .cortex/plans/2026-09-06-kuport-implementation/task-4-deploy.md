# Task 4 — Deploy manifests, Helm chart, samples

Repo root: `/home/nixos/repos/kuport`. All paths relative to it.

## Context

kuport is a Kubernetes operator that delivers a TCP or UDP port from one node's
addresses to a pod on another node, keeping the client's source address. It runs
as a single DaemonSet, one agent per node, with no central controller and no
leader election. Read `docs/spec.md` for the design; you need **Architecture**,
**Requirements of the cluster**, and **Deployment**.

You are shipping the two ways to install it. The plain manifests under `deploy/`
are the primary path, attached to each release with the image pinned by digest.
The Helm chart is the alternative for clusters where templating the class
objects and the image reference is easier than patching YAML.

Task 1 has landed the CRDs under `config/crd/`. The agent binary is
`kuport-agent`, image `ghcr.io/walzen-group/kuport-agent`. Tasks 5 and 6 are
running in parallel; the binary may not exist yet and that is fine — nothing you
write needs to run it.

## Target

Create:

- `deploy/namespace.yaml`
- `deploy/serviceaccount.yaml`
- `deploy/rbac.yaml` — ClusterRole and ClusterRoleBinding
- `deploy/daemonset.yaml`
- `deploy/kustomization.yaml`
- `chart/Chart.yaml`
- `chart/values.yaml`
- `chart/templates/` — the same objects, templated, plus `_helpers.tpl`
- `chart/crds/` — the CRDs, copied from `config/crd/`
- `chart/README.md`
- `config/samples/kuport.dev_v1alpha1_portmapclass.yaml`
- `config/samples/kuport.dev_v1alpha1_portmap.yaml`

Do not touch: `internal/`, `cmd/`, `config/crd/` (task 1 generates it; you copy
from it), `.github/`, `Dockerfile`, `docs/`, `README.md`, `flake.nix`, `go.mod`.

## Change

### 1. Namespace and ServiceAccount

Namespace `kuport-system`. ServiceAccount `kuport-agent` in it.

### 2. RBAC

The agent needs, cluster-wide:

| Resource | Verbs | Why |
|---|---|---|
| `kuport.dev/portmapclasses` | get, list, watch | the class definitions |
| `kuport.dev/portmapclasses/status` | get, update, patch | link claims and per-node rows |
| `kuport.dev/portmaps` | get, list, watch | the requests |
| `kuport.dev/portmaps/status` | get, update, patch | conditions and published addresses |
| `discovery.k8s.io/endpointslices` | get, list, watch | resolving serviceRef to pod addresses |
| `""/services` | get, list, watch | resolving the named port |
| `""/nodes` | get, list, watch | node labels for class selection, InternalIP for link endpoints |
| `""/namespaces` | get, list, watch | labels for namespaceSelector |
| `coordination.k8s.io/leases` | — | **not needed**, there is no leader election |

Grant nothing else. In particular no `create`/`delete` on Services or
EndpointSlices: kuport reads them and never touches them.

### 3. DaemonSet

The parts that are load-bearing, each for a stated reason:

```yaml
spec:
  template:
    spec:
      hostNetwork: true          # the rules are the node's, not a pod's
      dnsPolicy: ClusterFirstWithHostNet
      hostPID: false
      serviceAccountName: kuport-agent
      priorityClassName: system-node-critical
      tolerations:
        - operator: Exists       # every node the class may select, control planes included
      containers:
        - name: agent
          image: ghcr.io/walzen-group/kuport-agent:v0.1.0
          securityContext:
            privileged: false
            runAsNonRoot: false
            capabilities:
              add: [NET_ADMIN]
              drop: [ALL]
            allowPrivilegeEscalation: true
            readOnlyRootFilesystem: true
          resources:
            requests: {cpu: 10m, memory: 32Mi}
            limits: {memory: 128Mi}
```

`NET_ADMIN` is what lets it write nftables and netlink; it does not need
`privileged`. `readOnlyRootFilesystem` is safe because the image is a static
binary on scratch with nothing to write. No CPU limit — throttling a datapath
agent mid-reconcile is worse than the noisy-neighbour risk it prevents.

`tolerations: [{operator: Exists}]` is deliberate and it is the one place where
a broad toleration is right: the class's `nodeSelector` decides which nodes get
rules, and a taint must not silently remove a node the admin selected. Do **not**
add a nodeAffinity keeping the DaemonSet off control planes — an admin may well
want the public entry point there.

Give the container a readiness probe only if task 6 exposes an endpoint for it.
Check whether `cmd/kuport-agent` serves one before writing a probe that fails; if
it does not, leave the probe out and say so in your report rather than inventing
a port.

Set `updateStrategy.type: RollingUpdate` with `maxUnavailable: 1`. An agent
restart tears down its own rules on shutdown and rebuilds them on start, so a
whole-cluster simultaneous restart would drop every mapping at once.

### 4. Kustomization

`deploy/kustomization.yaml` lists the namespace, serviceaccount, rbac and
daemonset, plus `../config/crd`. Add an `images:` entry so
`kustomize edit set image` can pin the digest at release time — task 5's release
workflow does exactly that.

### 5. Helm chart

`Chart.yaml`: `apiVersion: v2`, `type: application`, name `kuport`,
`version` and `appVersion` both `0.1.0`. Task 5's release workflow rewrites both
from the git tag, so keep them on their own lines and do not template them.

CRDs go in `chart/crds/`, copied verbatim from `config/crd/`. Helm installs that
directory before the templates and never upgrades or deletes it, which is the
right behaviour for a CRD: an upgrade that dropped the CRD would delete every
PortMap in the cluster. Say so in `chart/README.md`, and say that a CRD schema
change needs `kubectl apply -f chart/crds/` by hand.

`values.yaml`, with every key documented by a short line above it:

```yaml
image:
  repository: ghcr.io/walzen-group/kuport-agent
  tag: ""            # defaults to .Chart.AppVersion
  digest: ""         # when set, wins over tag; this is what a release pins
  pullPolicy: IfNotPresent

nameOverride: ""
fullnameOverride: ""

rbac:
  create: true
serviceAccount:
  create: true
  name: ""

logLevel: info

resources:
  requests: {cpu: 10m, memory: 32Mi}
  limits: {memory: 128Mi}

tolerations:
  - operator: Exists
nodeSelector: {}

# PortMapClass objects to create with the release. Each entry is a class name
# mapped to a PortMapClassSpec, passed through as-is.
classes: {}
```

`classes` is the reason the chart earns its place over plain manifests: an admin
writes the class beside the install rather than as a second `kubectl apply`.
Render each entry as a `PortMapClass` whose `spec` is the value verbatim, so a
field added to the API later needs no chart change.

The image reference helper resolves digest first, then tag, then
`.Chart.AppVersion`:

```
{{- define "kuport.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}
```

Keep the templates a faithful mirror of `deploy/`. Where they differ, the plain
manifests win and the chart is wrong.

### 6. Samples

A `public` class matching the spec's example: one edge node by label, interfaces
`enp1s0` and `wt0`, ports 1024–65535 with 80 and 443 reserved, namespaceSelector
`kuport.dev/public: allowed`, `returnPath.mode: Vxlan` with the defaults.

An `intranet` class: every worker, interfaces `wt0` and `enp1s0`, no
namespaceSelector, `returnPath.mode: None` — a DaemonSet workload has a pod on
every accepting node, so no return path is ever programmed and `None` documents
that.

A `PortMap` for a UDP game server on 3000 in namespace `games`, and a second one
showing `endPort` with a range.

Each sample carries a two-line comment saying what it demonstrates. Not more.

## Constraints

1. **Toolchain is entered, never assumed.** No `helm`, `kubectl` or `kustomize`
   on the host PATH. Every command runs as `nix develop -c <cmd>` from the repo
   root.
2. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag. The wave's
   meta-agent makes one commit per task once that task's gate passes, so
   parallel delegates never race one git index.
3. **Stay inside your Target.** Do not edit `config/crd/` — copy from it.
4. **Look up current versions before pinning.** Chart apiVersion, any Helm
   library chart, the `system-node-critical` priority class name. Do not write a
   version or an API version from memory.
5. **Do not add a rule the platform already enforces.** Before writing any
   nodeAffinity, topology constraint, network policy or probe, name the concrete
   failure it prevents. If the default already prevents it, leave it out.
6. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`**, never `/tmp`, never
   `.cortex/`.
7. **Human-facing prose follows `catalyst-v2-writing-docs`**, humanizer pass
   included. That covers `chart/README.md` and every comment in `values.yaml`.
8. **Acceptance criteria are inviolable.** If one cannot be met, stop and report
   it with the criterion intact.
9. **Report as a diff, not a commit**: files changed, `git diff --stat`, verbatim
   gate output, deviations.
10. **Emission discipline**: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.

## Acceptance

From `/home/nixos/repos/kuport`. Paste verbatim output into your report.

```
nix develop -c kubectl apply --dry-run=client -f config/crd/ -f config/samples/
nix develop -c kubectl kustomize deploy/
nix develop -c kubectl apply --dry-run=client -k deploy/
nix develop -c helm lint chart/
nix develop -c helm template kuport chart/ > .tmp/render-default.yaml
nix develop -c helm template kuport chart/ --set image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000 > .tmp/render-digest.yaml
nix develop -c helm template kuport chart/ --set-json 'classes={"public":{"nodeSelector":{"matchLabels":{"kuport.dev/edge":"true"}},"interfaces":["enp1s0"],"returnPath":{"mode":"Vxlan"}}}' > .tmp/render-class.yaml
nix develop -c kubectl apply --dry-run=client -f .tmp/render-default.yaml
```

Green is: every dry-run listing objects as `(dry run)` with no error,
`helm lint` reporting `0 chart(s) failed`, and all three renders producing valid
YAML that the dry-run accepts.

**Negative checks**, all three required. Each one catches a template that
compiles but does the wrong thing:

- `grep '@sha256:' .tmp/render-digest.yaml` prints the image line. A digest that
  silently loses to the tag is the bug this catches.
- `grep -c 'kind: PortMapClass' .tmp/render-class.yaml` prints `1`, and
  `grep -c 'kind: PortMapClass' .tmp/render-default.yaml` prints `0`. An empty
  `classes` map must render no class at all, not an empty one.
- `diff <(nix develop -c kubectl kustomize deploy/ | grep -A5 'capabilities:') <(grep -A5 'capabilities:' .tmp/render-default.yaml)` shows the chart and the
  plain manifests granting the same capabilities. They are meant to be mirrors;
  a drift here is a real deployment difference.

## Report to

Your dispatch brief names the wave's meta-agent. Send your completion hand-back
to it with `c2d steer --agent <meta> "A2A: ..."`, not to the orchestrator.
