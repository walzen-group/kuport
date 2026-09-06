> **Status: ACTIVE** (2026-09-06)

# kuport implementation — execution plan

Execution skill: `catalyst-v2-orchestrating-delegates` (full lifecycle).
Each implementer receives **only its own task doc** plus the Global constraints
section below, reproduced into its spec. No delegate reads this index.

Repo root: `/home/nixos/repos/kuport` (git, branch `master`, no commits yet, no remote).

## Goal

Build kuport from `docs/spec.md`: a Kubernetes operator that delivers a TCP or
UDP port from one node's addresses to a pod on another node with the client's
source address intact. Ship it with CI, versioned releases, deploy manifests, a
Helm chart, and a guide for a future agent integrating it into a cluster.

## Architecture

One DaemonSet, one agent per node, no central controller and no leader election.
Each agent watches the API, computes the complete desired host state for its own
node, and applies it in one nftables transaction plus reconciled netlink objects.

```
PortMapClass ─┐
PortMap ──────┼─→ reconcile (pure) ─→ datapath.State ─→ nftables + netlink
EndpointSlice ┘                    └─→ status patches ─→ API server
```

The seam at `datapath.State` is the important one. Everything above it is pure
computation, testable with no cluster and no root. `internal/datapath` is the
only package permitted to touch the host.

## Tech stack

| Piece | Choice |
|---|---|
| Language | Go, toolchain pinned by `flake.nix` |
| nftables | `github.com/google/nftables` (netlink, never shelling out to `nft`) |
| netlink | `github.com/vishvananda/netlink` |
| Kubernetes client | `sigs.k8s.io/controller-runtime` |
| CRD generation | `sigs.k8s.io/controller-tools` (`controller-gen`) |
| Dependency provisioning | Nix flake, `nix develop -c <cmd>` |
| CI | GitHub Actions (`.github/workflows/`) |
| Image | `ghcr.io/walzen-group/kuport-agent`, scratch base, static binary |
| Module path | `github.com/walzen-group/kuport` |

Delegates look up the current version of every dependency before pinning it.
Do not write a version from memory.

## Source spec and resolved questions

Design doc: `docs/spec.md` in this repo. It is authoritative for the datapath,
and every rule in it carried real traffic by hand on a live cluster on
2026-09-06.

The spec's own "Open questions" section is **closed** by the user's decisions
below. Task 7 rewrites that section to record them. No delegate re-opens these.

| Question | Decision | Rationale |
|---|---|---|
| Port ranges | `spec.port` required; `spec.endPort` optional, inclusive. Mirrors NetworkPolicy's `port`/`endPort`. One nftables `dport <first>-<last>` match. | A single port needs no new field; a range needs no hundred objects. |
| No ready endpoint | Remove the DNAT rule. The port refuses (TCP reset, ICMP port-unreachable) rather than blackholing. `Programmed=False`, reason `NoReadyEndpoint`. | A client learns immediately. The spec's own lean. |
| Link address allocation | Recorded claim in `PortMapClass.status.links`, **not** a computed hash. `/31` per node pair. The endpoint-holding agent allocates and writes; the accepting agent reads. Optimistic concurrency (409 → re-read → redo) is the arbiter; no leader, no election, no peer protocol. | The API server is already a consistent store with compare-and-swap. Collisions become impossible and the allocation is visible in `kubectl get portmapclass -o yaml`. |
| Link allocation release | Freed after 24h unused. Any agent the class selects may run the GC. | The link *device* is torn down immediately on the next reconcile; the entry lingers only to hold the address reservation, so nothing in the free window carries traffic and the GC needs no locking. Clock skew is irrelevant against 24h. |
| Firewall | kuport writes `nat` and `mangle` in its own table and **never** anything in `filter`. It does not detect foreign filter rules either. `docs/operations.md` says a node with a host firewall needs the port opened there. | Writing the rule and detecting a foreign one have the same portability problem across nftables, iptables-legacy, firewalld and ufw, and a wrong "looks fine" is worse than no check. A workload-authored PortMap must not punch a hole in the host firewall. |
| CI platform | GitHub Actions, per the spec, not GitLab or Forgejo. | User's explicit choice on 2026-09-06 after the conflict was put to them. |
| Integration testing | No test cluster is available in this session. Unit and golden tests are written **and run**. envtest is written and run if its assets fetch; e2e is written and never run here. | User: "no testing available means theres no test cluster you can run it on as integration test". |

## Global constraints

Reproduce this section verbatim into every task spec.

1. **Toolchain is entered, never assumed.** There is no `go` on the host PATH.
   Every Go command runs as `nix develop -c <cmd>` from the repo root, or inside
   a shell entered with `nix develop`. A bare `go build` will fail.
2. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag. The wave's
   meta-agent makes one commit per task once that task's gate passes, so
   parallel delegates never race one git index.
3. **Append-only git discipline.** The branch tip only moves forward. No
   `git reset` in any mode, no `git rebase`, no `git commit --amend`, no history
   reordering, including on unpublished commits.
4. **Stay inside your Target.** Do not edit files another task owns. If you need
   a change in someone else's file, stop and report it.
5. **`internal/datapath` is the only package that touches the host.** No package
   above it opens a netlink socket, shells out, or reads `/proc`.
6. **Never shell out to `nft` or `ip`.** The predecessor did, and a node that
   rebooted while its package mirror was unreachable came up with no rules. The
   image is a static binary on scratch with no package manager.
7. **nftables reserves `mark` and `fwd` as chain names.** Every kuport chain name
   is prefixed `kup-`. This was found the hard way, twice.
8. **Look up current versions before pinning.** Providers, modules, actions,
   images, Go dependencies. Writing a version from memory pins the wrong schema.
9. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`** (gitignored), never
   `/tmp`, never `.cortex/`. That covers one-off scripts, logs, and intermediate
   data.
10. **Human-facing prose follows `catalyst-v2-writing-docs`**, humanizer pass
    included. Load the skill before writing or editing any README, doc, or
    comment block that reads as prose. This applies to task 7 in full and to
    anyone writing a package doc comment.
11. **Acceptance criteria are inviolable.** If one cannot be met, stop and report
    it with the criterion intact. Never descope the task's observable purpose.
12. **Report as a diff, not a commit**: files changed, `git diff --stat`,
    verbatim gate output, and any deviation from the spec.
13. **Emission discipline**: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes. A long silent reasoning turn stalls
    the wave invisibly.
14. **Report completion to the wave's meta-agent**, named in your dispatch brief,
    via `c2d steer --agent <meta> "A2A: ..."`. Not to the orchestrator.

## Pinned contracts

These are shared across tasks that run in parallel. They are **fixed**: a task
that wants one changed stops and reports rather than changing it.

### `datapath.State` — the seam

Package `internal/datapath`, file `state.go`. Task 2 owns this file; tasks 3 and
6 import it and must not edit it.

```go
package datapath

import "net/netip"

// State is the complete desired host state for ONE node. The reconcile computes
// it; Apply makes the host match it. A mapping that was deleted disappears by
// being absent from a later State, never by anything remembering to remove it.
type State struct {
	DNAT   []DNATRule
	Exempt []ExemptRule
	Mark   []MarkRule
	Links  []Link
	Rules  []IPRule
	Routes []Route
}

// PortSel is a protocol and an inclusive port range. Last == First for a single
// port, which is the common case.
type PortSel struct {
	Proto string // "tcp" or "udp"
	First uint16
	Last  uint16
}

// DNATRule runs on an accepting node: one per class interface per mapping.
type DNATRule struct {
	Iface  string
	Port   PortSel
	ToAddr netip.Addr
}

// ExemptRule is an identity SNAT that claims the connection before the CNI's
// masquerade can. Negate renders `oifname != <OifName>`.
type ExemptRule struct {
	OifName   string
	Negate    bool
	DstAddr   *netip.Addr
	SrcAddr   *netip.Addr
	Port      PortSel
	PortIsSrc bool
}

// MarkRule runs on a target node, marking replies so they route back through
// the accepting node rather than out this node's own uplink.
type MarkRule struct {
	SrcAddr netip.Addr
	Port    PortSel // matched as source port
	Mark    uint32
}

// Link is one point-to-point VXLAN to a peer node.
type Link struct {
	Name       string // kup-<first 8 hex of sha256(peer node name)>
	VNI        uint32
	Port       uint16
	LocalAddr  netip.Addr
	RemoteAddr netip.Addr
	LinkAddr   netip.Prefix // this node's end, a /31
}

type IPRule struct {
	Pref  uint32
	Mark  uint32
	To    *netip.Prefix // set only on the loop-guard rule
	Table uint32
}

type Route struct {
	Table uint32
	Via   netip.Addr
	Dev   string
}
```

### Naming and numbering

| Object | Value | Why |
|---|---|---|
| nftables table | `kuport` (family `ip`) | |
| nftables chains | `kup-pre`, `kup-post`, `kup-mangle` | `mark` and `fwd` are reserved words the parser rejects |
| `kup-pre` | `type nat hook prerouting priority dstnat - 10` | ahead of the CNI |
| `kup-post` | `type nat hook postrouting priority srcnat - 10` | must claim the connection before the CNI does |
| `kup-mangle` | `type filter hook prerouting priority mangle + 10` | |
| VXLAN link name | `kup-` + first 8 hex of `sha256(peer node name)` | 12 chars, under the 15-char ifname limit |
| Link addresses | `/31` out of `returnPath.vxlan.subnet`, slot recorded in class status | 128 slots in the default `/24`; RFC 3021 point-to-point |
| Routing table id | `200 + slot` | clear of `local` 255, `main` 254, `default` 253, and netbird's 100 |
| Packet mark | `0x6b700000 | slot` | clear of netbird's `0x1bd20` |
| `ip rule` prefs | loop guard `101`, divert `102`, for every link | above netbird's 110 and its resolve rule at 105, below `local` at 100. Linux permits duplicate prefs; the rules are told apart by their marks |

**The loop guard at pref 101 is not optional.** The VXLAN driver copies the
packet's mark onto the encapsulated packet, whose destination is the peer's
address. Without a rule resolving that destination out of `main` first, the outer
packet matches the divert rule and is routed back into the device it just left.
The kernel refuses and increments the device's `tx_errors` with no log line
anywhere. This cost real debugging time; do not "simplify" it away.

### API group and kinds

Group `kuport.wlz.li`, version `v1alpha1`. `PortMapClass` is cluster-scoped,
`PortMap` is namespaced. Full Go types are in task 1's doc; tasks 3 and 6 read
them from the generated source after task 1 lands.

Condition types and their reasons are fixed:

| Kind | Condition | True reason | False reasons |
|---|---|---|---|
| PortMap | `Accepted` | `Valid` | `ClassNotFound`, `NamespaceNotSelected`, `PortOutOfRange`, `PortReserved`, `PortConflict`, `InvalidPortRange` |
| PortMap | `Programmed` | `AllNodesReady` | `NoReadyEndpoint`, `ReturnPathUnavailable`, `NodeNotReady` (RemotePodMultipleAcceptingNodes removed 2026-09-06: single-serving makes it unreachable; AllNodesReady now means the serving node's rules exist and every required participant is ready) |
| PortMapClass | `Ready` | `Valid` | `TunnelModeRequired`, `VxlanPortConflict`, `SubnetExhausted`, `InvalidSubnet` |

## Revision notes

**2026-09-06, decisions closed.** Planning is complete, the open decisions are
settled, and wave 1 is clear to dispatch.

- An earlier note here said a dispatch had to come from a session rooted at
  `/home/nixos/repos/kuport`. That is narrower than it read. `c2d` takes an
  absolute `cwd` per agent and preflights that it exists, which is the documented
  fix for the cwd trap, so delegate tabs land in the kuport repo whatever the
  orchestrator's own root is. The orchestrator uses absolute paths for its own
  file tools.
- Mid-tier model is `opencode-go/qwen3.8-flash` at thinking xhigh, which the user
  named for subtasks. This departs from `catalyst-v2-model-picking`, whose table
  and `models.yaml` both assign `opencode-go/deepseek-v4-flash` at thinking max to
  mid-tier work, so it rides as a user directive and every such dispatch carries
  `"user_directive": true` beside the xhigh thinking level.

### Decisions closed before dispatch

| Decision | Answer |
|---|---|
| Go module path and image | `github.com/walzen-group/kuport` and `ghcr.io/walzen-group/kuport-agent` stand. The user's Forgejo at `forgejo.yuc.wlz.li/walzen-group` was offered and declined, so tasks 1, 4, 5 and 7 need no change |
| Commit policy | One commit per task, on branch `feat/initial-implementation`. The wave 1 meta-agent creates that branch with `git switch -c` before its first commit; HEAD is currently unborn on `master`, so the working tree carries over and `master` stays empty |
| Who commits | The wave's meta-agent, after that task's gate passes. Wave 2 runs four delegates in one working tree; four agents committing there would race the git index and sweep each other's staged files into the wrong commit |
| Meta-agent model | `opencode-go/deepseek-v4-flash` at thinking max, the `catalyst-v2-model-picking` default, confirmed by the user |

**2026-09-06, wave 1 landed.** Task 1 is committed at `78b40f5` on branch
`feat/initial-implementation`: 15 files, 1745 insertions. The recorded gate run
is in `task-1-gate-evidence.md`, including a server-side proof of the CEL
immutability rule on an envtest control plane, which is stronger than the
offline grep the spec asked for. Pinned: Go 1.26.7, controller-gen v0.22.0,
controller-runtime v0.25.0, `k8s.io/api` and `k8s.io/apimachinery` v0.37.0.

Two deviations were declared and accepted:

- `internal/api/v1alpha1` registers its types with the apimachinery
  `runtime.NewSchemeBuilder` plus a central `addKnownTypes`. controller-runtime
  v0.25.0 marks `scheme.Builder` deprecated and prescribes that exact
  replacement in the deprecation notice, so the api package keeps minimal
  dependencies. `AddToScheme` and `GroupVersion` keep their shape.
- A build-tagged `tools.go` holds the controller-runtime and `k8s.io/api` pins.
  After the first deviation nothing buildable imports them, so `go mod tidy`
  would drop the pins the spec required. The build tag keeps the file out of
  build, vet, and lint.

**2026-09-06, after the first real CI run.** The repo gained its GitHub remote
at `https://github.com/walzen-group/kuport` and the branch was pushed, so CI ran
for the first time. Two things came out of it, both dispatched as fixes.

- **CI run 34031346074 failed.** Three jobs passed (Check, Generated files match
  source, Image builds for linux/amd64). The job `Chart lints and renders` got
  past `helm lint` and `helm template`, then failed on
  `helm template kuport chart/ | kubectl apply --dry-run=client -f -` with
  `failed to download openapi: Get "http://localhost:8080/openapi/v2": connect:
  connection refused`. A kubectl client dry-run still contacts an API server for
  the schema it validates against, and a runner has no cluster. The plan
  recorded this behaviour three times for the local host and the CI step was
  written without it, which nothing caught because CI had never run.
- **API group changed to `kuport.wlz.li`.** The user controls `wlz.li` and does
  not control `kuport.dev`, and a Kubernetes API group should be a domain its
  author controls so groups from separate projects cannot collide in one
  cluster. `kuport.dev` came in from `docs/spec.md` with no recorded rationale.
  Settled while nothing is deployed: today it is a rename, and after objects
  exist in a cluster a different group is a different CRD and needs a migration.

## Cross-wave hand-forwards

Items one task reports and a later one acts on. Each wave's dispatch brief
carries the ones aimed at it.

| From | To | Item |
|---|---|---|
| task 5 | task 6 | The scratch image carries no CA bundle. If the agent needs outbound TLS to anything past the in-cluster API server, the image needs one. Task 6 decides and reports which |
| task 5 | task 4 | `release.yaml` greps the rendered manifests for `@sha256:` and fails loudly when `deploy/` image names disagree with the pushed ref. That grep is the contract check against task 4's manifests, so the two must agree |
| task 6 | tasks 1, 4 | A `configmaps` read rule on the ClusterRole (task 4's file) and an `addresses` field on `NodeStatus` (task 1's file). Task 6 reports both and makes neither; they land as a follow-up commit after wave 3 |
| task 3 | task 6 | `TunnelOK` and `CNIVxlanPort` were left off `Inputs`. The tunnel-mode and vxlan-port-collision class conditions need host access, so task 6 overlays them |
| task 3 | task 6 | `PublishedAddress.Address` is left empty. Resolving an interface to an address needs the host, so task 6 fills it |
| task 4 | task 6 | The DaemonSet carries no readiness or liveness probe, because no binary existed to probe. Once task 6 exposes `/readyz` and `/healthz`, they go into `deploy/daemonset.yaml` and the chart as a follow-up commit |
| task 5 | task 6 | The docker image gates (build, `--version`, the no-shell negative, size under 50MB) are blocked until `cmd/kuport-agent` exists. The exact commands are recorded in `task-5-gate-evidence.md` and run once task 6 lands |
| task 2 | task 7 | `docs/spec.md` prose says `/30` for link addresses; the pinned contract and the code use `/31`. Task 7 writes what the code does and flags the disagreement |
| task 2 | CI | `nft -c` needs root on this host: nftables 1.1.6 initializes a netfilter netlink cache even in check mode, so as uid 1000 it fails with `cache initialization failed: Operation not permitted`. The gate ran under `sudo -n`. Any CI invocation of that check has to account for it |

## Task table

| Doc | Task | Area | Depends on |
|---|---|---|---|
| `task-1-foundation.md` | Repo foundation: flake, module, API types, CRD generation | `flake.nix`, `go.mod`, `internal/api/v1alpha1/`, `config/crd/` | — |
| `task-2-datapath.md` | nftables + netlink rendering and apply, golden tests | `internal/datapath/` | 1 |
| `task-3-reconcile.md` | Pure desired-state computation, table-driven tests | `internal/reconcile/` | 1 |
| `task-4-deploy.md` | Deploy manifests and Helm chart | `deploy/`, `chart/`, `config/samples/` | 1 |
| `task-5-ci.md` | Dockerfile, CI workflow, release workflow | `Dockerfile`, `.github/workflows/` | 1 |
| `task-6-runtime.md` | Agent runtime: informers, status writing, link claims, main | `cmd/kuport-agent/`, `internal/agent/` | 1, 2, 3 |
| `task-7-docs.md` | Spec update with mermaid diagrams, operations guide, integration guide | `docs/`, `README.md` | 1–6 |

## Tracks

| Wave | Tasks | Parallel width |
|---|---|---|
| 1 | 1 | 1 |
| 2 | 2, 3, 4, 5 | 4 |
| 3 | 6, 7 | 2 |

Wave 2 is only parallel because `datapath.State` and the naming table above are
pinned here. Task 2 writes `state.go`; task 3 codes against this document's copy
of it and compiles once wave 2 converges.

## Agent allocation

Locked before dispatch. Frontier and meta-agent rows follow
`catalyst-v2-model-picking`; the mid-tier rows are the user's directive, which
that skill's table would otherwise fill with `opencode-go/deepseek-v4-flash` at
thinking max.

| Task | Tier | Runtime | Model |
|---|---|---|---|
| 1 foundation | Frontier | Claude Code | `claude-opus-4-8`, default effort |
| 2 datapath | Frontier | Claude Code | `claude-opus-4-8`, default effort |
| 3 reconcile | Frontier | Claude Code | `claude-opus-4-8`, default effort |
| 4 deploy | Mid-tier | omp | `opencode-go/qwen3.8-flash`, thinking xhigh |
| 5 ci | Mid-tier | omp | `opencode-go/qwen3.8-flash`, thinking xhigh |
| 6 runtime | Frontier | Claude Code | `claude-opus-4-8`, default effort |
| 7 docs | Mid-tier | omp | `opencode-go/qwen3.8-flash`, thinking xhigh |
| meta-agent, per wave | Mid-tier | omp | `opencode-go/deepseek-v4-flash`, thinking max |

Every `qwen3.8-flash` dispatch carries `"user_directive": true` beside its
`"thinking": "xhigh"`, which the tool requires for that level.

The Opus concurrency cap of 2 is respected: wave 2 runs exactly two Opus
delegates (tasks 2 and 3), wave 3 runs one (task 6).

Tasks 1, 2, 3 and 6 are frontier because each defines a contract others depend
on or carries logic whose failure mode is a silent packet drop. Tasks 4, 5 and 7
are fully specified mechanical work.

## Pre-work

No external status board. The task table above is the tracking, and the plan
directory is the record. Seven tasks in one repo over three waves does not earn
a board keeper for the epic's lifetime, and the user asked for the simplest
thing that works.

## Out of scope

- IPv6. Every rule is `table ip`.
- Admission webhooks. Conflicts are reported in status.
- HTTP routing or TLS.
- Load balancing one port across pods on several nodes.
- Several accepting nodes forwarding to one remote pod. Refused with
  `RemotePodMultipleAcceptingNodes`; the first accepting node by name is
  programmed and the others get the condition.
- Running the e2e script. No test cluster exists in this session.
- Anything in the `filter` table.
- Any change to `/home/nixos/repos/walzen-2-infra-test`. That repo is
  **read-only reference** for this epic.

## Whole-change verification

Run from `/home/nixos/repos/kuport`. The meta-agent runs these, not the workers.

```
nix develop -c go build ./...
nix develop -c go vet ./...
nix develop -c go test ./...
nix develop -c golangci-lint run
nix develop -c controller-gen crd paths=./internal/api/... output:crd:dir=/home/nixos/repos/kuport/.tmp/crd-check
diff -r config/crd /home/nixos/repos/kuport/.tmp/crd-check
nix develop -c helm lint chart/
nix develop -c helm template chart/ > /dev/null
nix develop -c kubectl apply --dry-run=client -f config/crd/ -f config/samples/
```

Green is: build and vet silent, `go test ./...` all `ok`, `golangci-lint` clean,
the CRD diff empty, `helm lint` reporting 0 failures, and the dry-run apply
listing every object as `(dry run)` with no error.

## Smoke test

No cluster, so the end-to-end observable is the rendered ruleset rather than a
packet. Task 2's golden tests are the smoke test:

Given a `PortMapClass` named `public` selecting node `edge-a` on interfaces
`enp1s0` and `wt0`, and a `PortMap` in namespace `games` asking for UDP 3000
against a Service whose only ready endpoint is pod `gs-0` at `10.244.18.107` on
node `worker-b`, the reconcile for node `edge-a` renders exactly:

```
table ip kuport {
  chain kup-pre {
    type nat hook prerouting priority dstnat - 10; policy accept;
    iifname "enp1s0" udp dport 3000 counter dnat to 10.244.18.107:3000
    iifname "wt0" udp dport 3000 counter dnat to 10.244.18.107:3000
  }
  chain kup-post {
    type nat hook postrouting priority srcnat - 10; policy accept;
    oifname "cilium_host" ip daddr 10.244.18.107 udp dport 3000 counter snat to ip saddr
  }
}
```

and for node `worker-b` renders the mark rule, the two `ip rule` entries and the
table route, with the reply exemption. Both are golden files in
`internal/datapath/testdata/`.

## 2026-09-06, final review adjudicated, fix wave planned

The whole-branch final review (report: `.cortex/reports/2026-09-06-whole-branch-final-review.md`,
hand-back: `.cortex/reports/handbacks/final-review.json`) closed with ten
findings F1-F10 and held the merge. CI on the pushed branch is green (run
34033426630). The claude orchestrator that received the hand-back hit its
usage limit; the session was reattached on 2026-09-06 (roster: `orchestrator`
= the resumed omp session, `orchestrator-old` = the parked claude session,
recorded in `.cortex/.sessions/2026-09-06-kuport-implementation.json`).

### User decisions (2026-09-06, quoted from the ask answers)

| Finding | Decision |
|---|---|
| F1/F3 (endpoint choice, multi-accept) | **One serving node, deterministic.** Every agent chooses the same endpoint by a global rule (drop local-wins: candidate on the lexicographically first accepting node, else smallest pod name). The mapping is served by exactly one accepting node: the one holding the chosen endpoint when it accepts, else the first accepting node by name. Every other accepting node reports `Programmed=False` reason `RemotePodMultipleAcceptingNodes`. |
| F2 (status contract) | **Implement to the pinned contract.** Agent host self-check writes real `Ready` + `Message` into its class node row; `Programmed=True` (reason `AllNodesReady`) only when every accepting node reports ready; `NodeNotReady` with a message otherwise; the claim-propagation window reports honestly and emits no reply-mark rule until the slot is resolved. |
| Sequencing | **Adjudicate, fix, then merge.** Fix wave first; merge `feat/initial-implementation` to master when green. |
| Chart publishing | **Add OCI chart push in the fix wave**: `release.yaml` pushes the packaged chart to `oci://ghcr.io/walzen-group/kuport`. |

### Wave 4 (tasks 8-10, one shared checkout, branch feat/initial-implementation)

| Doc | Task | Area | Depends on |
|---|---|---|---|
| `task-8-semantics.md` | Deterministic single-serving semantics + honest status (F1, F2, F3, F9, Service-watch removal) | `internal/reconcile/`, `internal/agent/`, RBAC services rule | — |
| `task-9-datapath.md` | Reconciled netlink links + apply ordering (F7, F10) | `internal/datapath/` | — |
| `task-10-deploy-ci.md` | Chart image tag, RBAC verbs, OCI chart push, envtest in CI (F5, F6, F8) | `chart/`, `deploy/`, `.github/workflows/` | — |

Wave 4 commits land as follow-up commits on `feat/initial-implementation`
(one per task, by the wave meta-agent after the task's gate passes, append-only).

### Wave 5 (task 11)

| Doc | Task | Area | Depends on |
|---|---|---|---|
| `task-11-docs.md` | Docs aligned to the adjudicated semantics (F1/F2/F3 docs, F8 claim, F5) | `docs/spec.md`, `docs/operations.md`, `docs/integration.md`, `README.md` | wave 4 |

### Out-of-scope amendments

The old out-of-scope note "Several accepting nodes forwarding to one remote
pod. Refused with RemotePodMultipleAcceptingNodes; the first accepting node by
name is programmed" narrows under the adjudication: the serving node is the
chosen pod's node when it accepts, else the first accepting node by name.
Docs task 11 rewrites the passages that stated the old readings.

### Merge gate after the fix wave

Master is unborn; the branch is pushed and CI-green. After wave 5 lands and
the whole-change gates pass, merge = create `master` from the branch tip and
push (per the user's fix-then-merge decision), then cut `v0.1.0` so the
release pipeline publishes image, chart (OCI), CRD bundle and rendered
manifests. Cutting the release is a separate user-go item after merge.

### Wave 4/5 agent allocation (2026-09-06)

User override, asked and answered: wave 4 runs entirely on
`opencode-go/qwen3.8-flash` at thinking max, workers and meta-agent alike,
because the claude account is at its session limit until 13:30 UTC and the
user chose this over the claude-opus-4-8 frontier tier and over the
models.yaml deepseek-v4-flash mid-tier default. This supersedes the earlier
qwen3.8-flash-at-xhigh subtask directive for this wave; every agent carries
`user_directive: true`. Wave 5 (docs) allocation is decided at its dispatch.

RBAC file split (single writer): task-8 removes the `corev1.Service` watch in
Go only; task-10 owns every RBAC file edit including the `services` rule
removal. The meta commits task-8's watch removal before task-10's RBAC hunk.

### 2026-09-06, wave 4 audit and second adjudication

Wave 4 landed and was pushed (tip 8fea956, CI run 34037491905). The meta
hand-back flagged the task-8 RPMAN interpretation (deviation 3) and the
Published scope (deviation 6) for audit. The orchestrator read the committed
decideRole/resolveProgrammed/publishedRows paths and put the fork to the
user, who decided:

**Serving node reports True.** A mapping on a class selecting 2+ accepting
nodes is served by exactly one node S and reports Programmed=True (reason
AllNodesReady, meaning redefined by task-11) once the participants (S, and
the holder when the chosen pod is remote) are ready and S's rules exist.
Sibling accepting nodes do not serve the mapping and do not gate it.
RemotePodMultipleAcceptingNodes retires from the PortMap Programmed false
reasons (pinned conditions table amended accordingly). Published narrows to
S's interfaces and addresses.

Wave 4.1 (one worker fix-serving-status + meta-wave41, qwen3.8-flash max per
the standing override) executes `task-8b-serving-true.md`. Wave 5 (task-11
docs) dispatches after it lands, and the merge gate then holds on the
whole-change gates green at the pushed tip.

### 2026-09-06, wave 4.1 closed

Task-8b committed as 8d68d2c and pushed (CI run on it). Whole-change battery
green at the tip; red run recorded against the 879f902 surface. Wave 5
(task-11 docs) is dispatched next; its spec carries the post-4.1 items
(RPMAN doc rows, AllNodesReady meaning, Published comment + CRD regen, holder
gate description). Merge gate holds until wave 5 lands and gates are green on
the pushed tip.
