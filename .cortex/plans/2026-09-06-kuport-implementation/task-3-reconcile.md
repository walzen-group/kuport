# Task 3 — Reconcile: pure desired-state computation

Repo root: `/home/nixos/repos/kuport`. All paths relative to it.

## Context

kuport delivers a TCP or UDP port from one node's addresses to a pod on another
node, keeping the client's source address. Read `docs/spec.md` first, especially
**Architecture** and **The reconcile**.

There is no central controller and no leader election. One DaemonSet, one agent
per node, and every agent computes from the same inputs with the same
deterministic tie-breaks so they agree without talking to each other. You are
writing that computation, as a pure function: no API calls, no clock reads except
one injected `now`, no host access, no goroutines. Task 6 wraps it in informers
and applies what you return; task 2 turns your `datapath.State` into rules.

This package is where endpoint choice, conflict ordering, class selection and
every refusal live, and it should carry the bulk of the repo's tests.

Task 1 landed the module, the flake and the API types. Task 2 is writing
`internal/datapath` in parallel with you; `state.go` is reproduced below and is
a **pinned contract** you code against.

## Target

Create:

- `internal/reconcile/reconcile.go` — `Compute(in Inputs) Result`
- `internal/reconcile/endpoints.go` — endpoint selection
- `internal/reconcile/conflicts.go` — port interval conflict resolution
- `internal/reconcile/links.go` — link slot allocation and GC
- `internal/reconcile/status.go` — status construction
- `internal/reconcile/*_test.go` — table-driven, this is the bulk of the work

Do not touch: `internal/datapath/` (task 2 owns every file in it, including
`state.go`), `internal/agent/`, `cmd/`, `config/`, `deploy/`, `chart/`, `docs/`,
`README.md`, `flake.nix`.

## The pinned `datapath.State`

Task 2 is writing this file right now. Import it; do not create it, do not edit
it. If you believe it needs a change, **stop and report it**.

```go
package datapath

import "net/netip"

type State struct {
	DNAT   []DNATRule
	Exempt []ExemptRule
	Mark   []MarkRule
	Links  []Link
	Rules  []IPRule
	Routes []Route
}

type PortSel struct {
	Proto string // "tcp" or "udp"
	First uint16
	Last  uint16
}

type DNATRule struct {
	Iface  string
	Port   PortSel
	ToAddr netip.Addr
}

type ExemptRule struct {
	OifName   string
	Negate    bool
	DstAddr   *netip.Addr
	SrcAddr   *netip.Addr
	Port      PortSel
	PortIsSrc bool
}

type MarkRule struct {
	SrcAddr netip.Addr
	Port    PortSel
	Mark    uint32
}

type Link struct {
	Name       string
	VNI        uint32
	Port       uint16
	LocalAddr  netip.Addr
	RemoteAddr netip.Addr
	LinkAddr   netip.Prefix
}

type IPRule struct {
	Pref  uint32
	Mark  uint32
	To    *netip.Prefix
	Table uint32
}

type Route struct {
	Table uint32
	Via   netip.Addr
	Dev   string
}
```

## Change

### 1. The signature

```go
package reconcile

type Inputs struct {
	NodeName   string
	Nodes      []*corev1.Node            // labels, and addresses for link endpoints
	Namespaces []*corev1.Namespace       // labels, for namespaceSelector
	Classes    []*v1alpha1.PortMapClass
	PortMaps   []*v1alpha1.PortMap
	Slices     []*discoveryv1.EndpointSlice
	Now        metav1.Time               // injected; never call time.Now()
}

type Result struct {
	// What this node's host should look like. Hand straight to datapath.Apply.
	State datapath.State

	// Status for the PortMaps this node owns, keyed by namespace/name. A node
	// owns a PortMap when it holds the chosen endpoint. Empty for every other
	// PortMap, so exactly one agent writes each.
	PortMapStatus map[types.NamespacedName]v1alpha1.PortMapStatus

	// Per-class status this node contributes: its own Nodes row always, plus
	// Links entries it is claiming or releasing.
	ClassStatus map[string]ClassContribution
}

type ClassContribution struct {
	Node       v1alpha1.NodeStatus
	ClaimLinks []v1alpha1.LinkAllocation // to add or update
	DropLinks  []string                  // Key values to remove
	Conditions []metav1.Condition
}
```

`Compute` is pure: same `Inputs`, same `Result`, byte for byte, every time. No
map iteration order may leak into output — sort before you emit.

### 2. Class selection

A class selects this node when `spec.nodeSelector` matches the node's labels.
Use `metav1.LabelSelectorAsSelector`. An empty selector matches everything, per
apimachinery's own semantics — do not special-case it.

A PortMap may reference a class only when `spec.namespaceSelector` matches its
namespace's labels. A nil or empty selector selects every namespace.

### 3. Validation and the `Accepted` condition

In this order. First failure wins and sets `Accepted=False`:

| Check | Reason on failure |
|---|---|
| the named class exists | `ClassNotFound` |
| `endPort`, if set, is >= `port` | `InvalidPortRange` |
| the class's namespaceSelector selects the PortMap's namespace | `NamespaceNotSelected` |
| every port in `[port, endPort]` is within `[ports.min, ports.max]` | `PortOutOfRange` |
| no port in `[port, endPort]` appears in `ports.reserved` | `PortReserved` |
| no earlier PortMap on the same class holds an overlapping interval on the same protocol | `PortConflict` |

`ports` absent means min 1, max 65535, no reserved. `endPort` absent means the
interval is the single port.

### 4. Conflict ordering — deterministic across agents

Two PortMaps conflict when they name the same class, the same protocol, and
their `[port, endPort]` intervals overlap. The **earlier** one is accepted and
the later one gets `PortConflict`.

"Earlier" is: `metadata.creationTimestamp` ascending; ties broken by
`namespace/name` ascending, bytewise. Creation timestamps have one-second
resolution, so ties are common and the tiebreak is not decorative — every agent
must land on the same winner or two nodes program different pods.

The message on the losing PortMap names the winner:
`port 3000 is held by games/gameserver`.

### 5. Endpoint selection

Resolve `serviceRef` to the EndpointSlices labelled
`kubernetes.io/service-name=<serviceRef.name>` in the PortMap's namespace, then
to the port named `serviceRef.port`.

An endpoint is a candidate when `conditions.ready` is true (a nil `ready` means
ready, per the EndpointSlice contract) and it has at least one address of
`addressType: IPv4`.

Choose one:

1. A candidate whose `nodeName` equals `in.NodeName` wins.
2. Otherwise the first candidate by `targetRef.name` ascending, bytewise.

Rule 2 is what makes every agent agree. Do not use slice order; slices arrive in
arbitrary order and are not stable across watches.

Zero candidates: emit **no DNAT rule at all** for that mapping,
`Programmed=False` with reason `NoReadyEndpoint`. The port then refuses — TCP
reset, ICMP port-unreachable — rather than blackholing. This is a settled
decision; do not keep a stale rule to smooth a rollout.

### 6. Rules for this node

For each accepted PortMap whose class selects this node (this node is an
accepting node):

- One `DNATRule` per interface in `spec.interfaces`, with
  `Port{Proto: lower(protocol), First: port, Last: endPort or port}` and
  `ToAddr` the chosen endpoint address.
- One `ExemptRule`: `OifName: "cilium_host"`, `Negate: false`,
  `DstAddr: &endpoint`, same `Port`, `PortIsSrc: false`.
- If the chosen endpoint is on another node, ensure a link to that node and emit
  nothing else here. The other node's agent owns its half.

For each accepted PortMap whose chosen endpoint is on this node and whose
accepting node is elsewhere (this node is a target node):

- One `MarkRule`: `SrcAddr` the endpoint, `Port` matched as source port
  (`PortIsSrc` semantics: the mark rule always matches sport), `Mark` from the
  link's slot.
- One `ExemptRule`: `OifName: "cilium_*"`, `Negate: true`,
  `SrcAddr: &endpoint`, same `Port`, `PortIsSrc: true`.
- The link, the two `IPRule`s and the `Route`.

A mapping whose accepting node **is** this node and whose pod is also here gets
the DNAT and the exemption only: no link, no mark, no rules, no route.

### 7. Links, slots and numbering

A link is needed for a node pair when at least one accepted PortMap has its
accepting node at one end and its chosen endpoint at the other.

Allocation is a **recorded claim**, not a computed hash. The API server's
optimistic concurrency is the arbiter; there is no leader and no agent-to-agent
protocol.

- Read existing claims from `class.status.links`. A claim's `Key` is the two
  node names sorted and joined with `/`.
- If a needed pair already has a claim, use its `Slot`. Never renumber an
  existing claim.
- If it does not, allocate the **lowest free slot** and emit it in `ClaimLinks`.
  Only the agent holding the chosen endpoint proposes a new claim — that is the
  same single-writer rule that governs PortMap status, so it needs no election.
- Slots index `/31`s within `class.spec.returnPath.vxlan.subnet`. The default
  `169.254.77.0/24` gives 128 slots. No free slot is `SubnetExhausted` on the
  class.

Derived, all fixed:

| Thing | Value |
|---|---|
| link `/31` | slot-th `/31` of the class subnet |
| this node's address | the lower of the pair's two node names takes the first address of the `/31`, the other takes the second |
| routing table id | `200 + slot` |
| packet mark | `0x6b700000 | uint32(slot)` |
| link name | `"kup-" + hex(sha256(peer node name))[:8]` |
| `IPRule` loop guard | `Pref: 101`, `Mark: mark`, `To: &<peer node address>/32`, `Table: 254` (main) |
| `IPRule` divert | `Pref: 102`, `Mark: mark`, `To: nil`, `Table: 200+slot` |
| `Route` | `Table: 200+slot`, `Via: <peer's end of the /31>`, `Dev: <link name>` |

The node address at each end of the VXLAN outer header is the node's
`InternalIP` from `status.addresses`. The loop guard at pref 101 is not optional
and not simplifiable: the VXLAN driver copies the mark onto the encapsulated
packet, which then matches the divert rule and is routed back into the device it
just left. The kernel refuses silently and bumps `tx_errors`.

**GC.** For each existing claim:

- The pair is needed → clear `UnusedSince` (emit an updated claim).
- The pair is not needed → set `UnusedSince` to `in.Now` if it is not already
  set.
- `UnusedSince` is more than 24h before `in.Now` → emit its `Key` in
  `DropLinks`.

Any agent the class selects runs this GC; the write conflicts sort themselves
out. Note that the link *device* disappears the moment the pair is unneeded,
because it is simply absent from `State.Links`. The claim lingers only to hold
the address reservation, which is why the GC needs no locking.

### 8. Refusals

| Condition | Behaviour |
|---|---|
| `returnPath.mode: None` and the chosen pod is on another node | `Programmed=False`, `ReturnPathUnavailable`, no rules for that mapping |
| a class selects several accepting nodes and a mapping's pod is remote | program the **first accepting node by name** only; every other accepting node emits no rules and the PortMap gets `Programmed=False`, `RemotePodMultipleAcceptingNodes` |

The second is a real limitation, not an oversight. With two accepting nodes
forwarding to one remote pod, that pod's node has to send each reply back to
whichever node forwarded it, and the reply carries nothing that says which.
Making it work needs a distinct port per accepting node and a return rule
matching source port.

Cilium tunnel-mode detection and VXLAN port collision are **task 6's**, not
yours — they need host access. Accept a `TunnelOK bool` and `CNIVxlanPort int`
on `Inputs` if you want them factored in; otherwise leave them to task 6 to
overlay onto the class conditions.

### 9. Status

`PortMapStatus` is emitted **only** for PortMaps whose chosen endpoint is on
this node. That gives exactly one writer per PortMap without leader election,
and it changes hands when the endpoint moves.

`Published` lists one row per accepting node and interface, with the address the
port answers on — resolve each accepting node's interface to an address from
that node's `status.addresses` where possible, and leave `Address` empty where
it cannot be resolved from the API alone (task 6 fills it from the host).

`ObservedGeneration` is the PortMap's `metadata.generation`.

## Constraints

1. **Toolchain is entered, never assumed.** No `go` on the host PATH. Every Go
   command runs as `nix develop -c <cmd>` from the repo root.
2. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag. The wave's
   meta-agent makes one commit per task once that task's gate passes, so
   parallel delegates never race one git index.
3. **Stay inside your Target.** `internal/datapath/state.go` belongs to task 2.
   If it has not landed yet, write your code against the copy in this doc and
   compile once it appears; do not create the file yourself.
4. **`Compute` is pure.** No API calls, no `time.Now()`, no host access, no
   goroutines, no logging to a package-level logger. Everything it needs arrives
   in `Inputs`.
5. **Determinism is a correctness property, not a nicety.** Every agent computes
   independently and they must agree. Sort every output slice. Test with
   `-count=10` and with shuffled input order.
6. **Look up current versions before pinning** any dependency you add.
7. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`**, never `/tmp`, never
   `.cortex/`.
8. **Human-facing prose follows `catalyst-v2-writing-docs`**, humanizer pass
   included, for package doc comments that run to prose.
9. **Acceptance criteria are inviolable.** If one cannot be met, stop and report
   it with the criterion intact.
10. **Report as a diff, not a commit**: files changed, `git diff --stat`,
    verbatim gate output, deviations.
11. **Emission discipline**: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.

## Acceptance

From `/home/nixos/repos/kuport`. Paste verbatim output into your report.

```
nix develop -c go build ./...
nix develop -c go vet ./...
nix develop -c go test ./internal/reconcile/... -v -count=1
nix develop -c go test ./internal/reconcile/... -count=10
nix develop -c golangci-lint run ./internal/reconcile/...
```

Table-driven cases required, at minimum one per row:

| Area | Cases |
|---|---|
| class selection | selects this node; does not; empty nodeSelector |
| namespace gating | selected; not selected; nil selector |
| port validation | in range; below min; above max; reserved; `endPort < port`; range straddling a reserved port |
| conflicts | disjoint intervals coexist; overlapping intervals, earlier wins; **same creationTimestamp, name tiebreak**; different protocols on the same port coexist |
| endpoint choice | local pod wins over remote; no local pod picks lowest `targetRef.name`; unready endpoints skipped; nil `ready` treated as ready; zero candidates |
| roles | accepting only; target only; both on one node; neither |
| links | fresh claim takes lowest free slot; existing claim reused unchanged; subnet exhausted; `/31` address split by node name order |
| link GC | needed pair clears `UnusedSince`; unneeded pair sets it; 24h-old entry dropped; 23h-old entry kept |
| refusals | `mode: None` with a remote pod; two accepting nodes with a remote pod |
| status | emitted only by the endpoint holder; `Published` one row per node per interface |

**Negative checks**, both required:

- **Determinism under shuffle.** A test that shuffles `Inputs` slices with a
  seeded RNG, runs `Compute` 20 times, and asserts every `Result` is deeply
  equal to the first. Sorting bugs are the whole risk in this package and a
  single-run test will not find them.
- **Agent agreement.** For one fixture with three nodes, run `Compute` once per
  node and assert the three agree on the chosen endpoint, the accepting node,
  and the link slot. Two agents disagreeing is the failure mode that takes the
  port down with every rule looking correct.

Green is: build and vet silent, every test `ok` at both `-count=1` and
`-count=10`, `golangci-lint` clean.

## Report to

Your dispatch brief names the wave's meta-agent. Send your completion hand-back
to it with `c2d steer --agent <meta> "A2A: ..."`, not to the orchestrator.
