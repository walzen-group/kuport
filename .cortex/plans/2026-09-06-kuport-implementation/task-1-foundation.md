# Task 1 — Repo foundation: flake, module, API types, CRD generation

Repo root: `/home/nixos/repos/kuport`. All paths below are relative to it.

## Context

kuport is a Kubernetes operator that delivers a TCP or UDP port from one node's
addresses to a pod on another node, keeping the client's source address. The
design is written and measured in `docs/spec.md` — read it before you start; it
is authoritative for everything except the four questions the plan closed (see
Decisions below, which override the spec's "Open questions" section).

The repo is empty apart from `README.md` and `docs/spec.md`. Nothing is built.
You are laying the foundation every other task builds on: the Nix toolchain, the
Go module, the API types, and the generated CRDs. Six other tasks are blocked
on yours, so the contract you write is the one they code against.

There is **no `go` on the host PATH.** The flake you write is how everyone,
including you, gets one.

## Target

Create:

- `flake.nix`, `flake.lock`, `.envrc`
- `.gitignore`
- `go.mod`, `go.sum` — module `github.com/walzen-group/kuport`
- `internal/api/v1alpha1/` — `groupversion_info.go`, `portmapclass_types.go`,
  `portmap_types.go`, `zz_generated.deepcopy.go`
- `config/crd/` — generated CRD manifests
- `Makefile` — thin, only the generate and verify targets named below

Do not create: anything under `internal/datapath/`, `internal/reconcile/`,
`internal/agent/`, `cmd/`, `deploy/`, `chart/`, `.github/`, `Dockerfile`. Other
tasks own those. Do not edit `docs/spec.md` or `README.md`; task 7 owns them.

## Change

### 1. The flake

`flake.nix` provides a devShell with everything the whole project needs, because
every other task calls `nix develop -c <cmd>` against it. Pin `nixpkgs` to
`nixos-unstable` and commit a `flake.lock`.

The shell must provide, at minimum: `go` (1.26 or newer), `gopls`,
`golangci-lint`, `controller-gen` (from `kubernetes-controller-tools`),
`kubectl`, `kustomize`, `helm`, `setup-envtest` (from `setup-envtest` or
`kubebuilder`), `git`, and `skopeo` or `docker` is **not** needed.

Check the current attribute name for each of these in nixpkgs before writing it.
Several have moved (`controller-tools` vs `kubernetes-controller-tools`), and a
wrong attribute makes the shell fail to evaluate for six other agents.

Add `.envrc` containing `use flake`.

Verify the shell evaluates and that each tool is on PATH:

```
cd /home/nixos/repos/kuport
nix develop -c bash -c 'go version && controller-gen --version && golangci-lint --version && helm version --short && kubectl version --client'
```

If a package is genuinely unavailable in nixpkgs, say so in your report and
leave it out rather than pinning a broken attribute. Do not substitute a
`go install` at shell entry — the point of the flake is that nothing is fetched
at use time.

### 2. `.gitignore`

At minimum: `.tmp/`, `bin/`, `result`, `result-*`, `.direnv/`, `dist/`.

### 3. The Go module

```
nix develop -c go mod init github.com/walzen-group/kuport
```

Add `k8s.io/apimachinery`, `k8s.io/api`, and `sigs.k8s.io/controller-runtime`.
Look up the current release of controller-runtime and take the matching
`k8s.io/*` minor. Do not write a version from memory.

### 4. API types

Group `kuport.dev`, version `v1alpha1`. `PortMapClass` is **cluster-scoped**,
`PortMap` is **namespaced**. Use kubebuilder markers throughout; `controller-gen`
generates both the CRDs and the deepcopy functions.

`groupversion_info.go` is the standard scheme-builder file with
`// +kubebuilder:object:generate=true` and `// +groupName=kuport.dev`.

#### `portmapclass_types.go`

```go
type PortMapClassSpec struct {
	// Nodes that accept traffic for this class. Every matching node gets rules.
	// +kubebuilder:validation:Required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// Interface names the DNAT rules match on, per accepting node. One rule per
	// interface per mapping.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Interfaces []string `json:"interfaces"`

	// +optional
	Ports *PortRange `json:"ports,omitempty"`

	// Which namespaces may reference this class. Empty selects every namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// +kubebuilder:validation:Required
	ReturnPath ReturnPath `json:"returnPath"`
}

type PortRange struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=1
	// +optional
	Min int32 `json:"min,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=65535
	// +optional
	Max int32 `json:"max,omitempty"`

	// Ports the admin keeps back. A PortMap naming one is rejected in status.
	// +optional
	Reserved []int32 `json:"reserved,omitempty"`
}

// +kubebuilder:validation:Enum=Vxlan;None
type ReturnPathMode string

const (
	ReturnPathVxlan ReturnPathMode = "Vxlan"
	ReturnPathNone  ReturnPathMode = "None"
)

type ReturnPath struct {
	// None refuses any mapping whose pod is on another node.
	// +kubebuilder:validation:Required
	Mode ReturnPathMode `json:"mode"`

	// +optional
	Vxlan *VxlanConfig `json:"vxlan,omitempty"`
}

type VxlanConfig struct {
	// +kubebuilder:default=4242
	// +optional
	VNI int32 `json:"vni,omitempty"`

	// Must differ from the CNI's, which is 8472 for Cilium.
	// +kubebuilder:default=4790
	// +optional
	Port int32 `json:"port,omitempty"`

	// Link addresses are allocated from here, a /31 per node pair.
	// +kubebuilder:default="169.254.77.0/24"
	// +optional
	Subnet string `json:"subnet,omitempty"`
}
```

Status. Note `links` — this is the recorded-claim allocation, written by the
agent holding a mapping's endpoint and read by the accepting node's agent:

```go
type PortMapClassStatus struct {
	// One entry per node pair that has, or recently had, a return link.
	// +optional
	// +listType=map
	// +listMapKey=key
	Links []LinkAllocation `json:"links,omitempty"`

	// One row per node this class selects, maintained by that node's agent.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []NodeStatus `json:"nodes,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type LinkAllocation struct {
	// The two node names joined by a slash, sorted. The list map key.
	// +kubebuilder:validation:Required
	Key string `json:"key"`

	// The two node names, sorted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	Peers []string `json:"peers"`

	// The allocated /31 out of the class subnet.
	// +kubebuilder:validation:Required
	Subnet string `json:"subnet"`

	// Zero-based index of this /31 within the class subnet. Fixes the routing
	// table id (200 + slot) and the packet mark (0x6b700000 | slot).
	// +kubebuilder:validation:Required
	Slot int32 `json:"slot"`

	// Set when no PortMap needs this pair. The entry is dropped once this is
	// more than 24h old. Absent while the pair is in use.
	// +optional
	UnusedSince *metav1.Time `json:"unusedSince,omitempty"`
}

type NodeStatus struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Ready bool `json:"ready"`

	// Why the node is not ready, when it is not.
	// +optional
	Message string `json:"message,omitempty"`

	// MTU of the interface carrying the return links, as read from the host.
	// +optional
	UnderlayMTU int32 `json:"underlayMTU,omitempty"`

	// UnderlayMTU minus the 50-byte VXLAN overhead. A reply larger than this
	// cannot cross a return link.
	// +optional
	LinkMTU int32 `json:"linkMTU,omitempty"`
}
```

Markers on the type: `+kubebuilder:object:root=true`,
`+kubebuilder:resource:scope=Cluster,shortName=pmc`,
`+kubebuilder:subresource:status`, and print columns for `MODE`
(`.spec.returnPath.mode`), `INTERFACES` (`.spec.interfaces`) and `AGE`.

#### `portmap_types.go`

```go
type PortMapSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClassName string `json:"className"`

	// +kubebuilder:validation:Required
	Protocol Protocol `json:"protocol"`

	// The port clients dial on an accepting node, and the port the pod
	// receives on. With EndPort set, the first port of an inclusive range.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Last port of an inclusive range starting at Port. Omit for a single port.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	EndPort *int32 `json:"endPort,omitempty"`

	// +kubebuilder:validation:Required
	ServiceRef ServiceRef `json:"serviceRef"`
}

// +kubebuilder:validation:Enum=TCP;UDP
type Protocol string

type ServiceRef struct {
	// A Service in the same namespace. Any type, ClusterIP included. It exists
	// so the agent has endpoints to follow and a named port to resolve.
	// kuport never modifies it.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// The named port on that Service.
	// +kubebuilder:validation:Required
	Port string `json:"port"`
}
```

Immutability is enforced with CEL, one marker per field, on the spec struct:

```go
// +kubebuilder:validation:XValidation:rule="self.className == oldSelf.className",message="className is immutable"
// +kubebuilder:validation:XValidation:rule="self.protocol == oldSelf.protocol",message="protocol is immutable"
// +kubebuilder:validation:XValidation:rule="self.port == oldSelf.port",message="port is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.endPort) == has(oldSelf.endPort) && (!has(self.endPort) || self.endPort == oldSelf.endPort)",message="endPort is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.endPort) || self.endPort >= self.port",message="endPort must be greater than or equal to port"
```

They are immutable because changing one is indistinguishable from deleting a
mapping and creating another, and the reconcile is simpler if it never has to
unwind a half-changed mapping.

Status:

```go
type PortMapStatus struct {
	// +optional
	Endpoint *Endpoint `json:"endpoint,omitempty"`

	// The addresses this port answers on right now, one row per accepting node
	// and interface. This is what a person needs from `kubectl get portmap`.
	// +optional
	Published []PublishedAddress `json:"published,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type Endpoint struct {
	Node    string `json:"node"`
	Address string `json:"address"`
}

type PublishedAddress struct {
	Node      string `json:"node"`
	Interface string `json:"interface"`
	Address   string `json:"address"`
}
```

Markers: `+kubebuilder:object:root=true`,
`+kubebuilder:resource:scope=Namespaced,shortName=pm`,
`+kubebuilder:subresource:status`, print columns for `CLASS`, `PROTO`, `PORT`,
`ENDPOINT` (`.status.endpoint.address`), `AGE`, and a wide column for
`PUBLISHED` (`.status.published[*].address`).

### 5. Condition constants

Add a `conditions.go` in the same package with exported string constants for
every condition type and reason in this table. Tasks 3 and 6 use these; do not
make them retype the strings.

| Kind | Condition | True reason | False reasons |
|---|---|---|---|
| PortMap | `Accepted` | `Valid` | `ClassNotFound`, `NamespaceNotSelected`, `PortOutOfRange`, `PortReserved`, `PortConflict`, `InvalidPortRange` |
| PortMap | `Programmed` | `AllNodesReady` | `NoReadyEndpoint`, `ReturnPathUnavailable`, `NodeNotReady`, `RemotePodMultipleAcceptingNodes` |
| PortMapClass | `Ready` | `Valid` | `TunnelModeRequired`, `VxlanPortConflict`, `SubnetExhausted`, `InvalidSubnet` |

### 6. Generation and the Makefile

```
make generate   # controller-gen object paths=./internal/api/...
make manifests  # controller-gen crd paths=./internal/api/... output:crd:dir=config/crd
make verify     # regenerate into .tmp/ and diff against config/crd; non-zero on drift
```

Every target runs its tool through `nix develop -c`. `make verify` is what CI
calls, so generated files cannot drift from source.

## Constraints

1. **Toolchain is entered, never assumed.** There is no `go` on the host PATH.
   Every Go command runs as `nix develop -c <cmd>` from the repo root. A bare
   `go build` will fail.
2. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag. The wave's
   meta-agent makes one commit per task once that task's gate passes, so
   parallel delegates never race one git index.
3. **Stay inside your Target.** Do not create or edit files another task owns.
4. **Look up current versions before pinning.** Go dependencies, nixpkgs
   attribute names, the controller-runtime release. Writing a version from
   memory pins the wrong schema.
5. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`** (gitignored), never
   `/tmp`, never `.cortex/`.
6. **Human-facing prose follows `catalyst-v2-writing-docs`**, humanizer pass
   included. Load the skill before writing any package doc comment that runs to
   prose. Field comments in the sketches above are already in the right register:
   say what the field is and why, no marketing, no filler.
7. **Acceptance criteria are inviolable.** If one cannot be met, stop and report
   it with the criterion intact. Never descope.
8. **Report as a diff, not a commit**: files changed, `git diff --stat`, verbatim
   gate output, deviations.
9. **Emission discipline**: at most 15 minutes of survey before your first file
   write, then a write every ~10 minutes.

## Acceptance

Every command from `/home/nixos/repos/kuport`. Paste the verbatim output of each
into your report.

```
nix develop -c bash -c 'go version && controller-gen --version && golangci-lint --version && helm version --short'
nix develop -c go build ./...
nix develop -c go vet ./...
nix develop -c golangci-lint run
make generate
make manifests
make verify
nix develop -c kubectl apply --dry-run=client -f config/crd/
```

Green is: every tool reporting a version, build and vet silent,
`golangci-lint` clean, `make verify` exiting 0, and the dry-run apply printing
`customresourcedefinition.apiextensions.k8s.io/portmapclasses.kuport.dev created
(dry run)` and the same for `portmaps.kuport.dev`.

**Negative check.** Confirm the immutability CEL actually bites rather than just
compiling. Write a PortMap manifest to `.tmp/`, then:

```
nix develop -c kubectl apply --dry-run=server -f .tmp/pm.yaml
```

A server dry-run needs a cluster and you do not have one, so instead assert the
rule reached the CRD:

```
grep -c 'className is immutable' config/crd/*portmaps*.yaml
```

That must print `1` or more. A CEL rule that never made it into the generated
CRD is the failure this check catches.

## Report to

Your dispatch brief names the wave's meta-agent. Send your completion hand-back
to it with `c2d steer --agent <meta> "A2A: ..."`, not to the orchestrator.
