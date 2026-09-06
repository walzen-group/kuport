# task-8 — Deterministic single-serving semantics and honest status (F1, F2, F3, F9, Service-watch removal)

Execution skill: `catalyst-v2-sdd-rules` for the test-first steps; global
constraints section below reproduced from the plan index.

## Context

The final whole-branch review of `feat/initial-implementation` (report:
`.cortex/reports/2026-09-06-whole-branch-final-review.md`) found three blocking
defects and two hygiene items in the reconcile/agent code, all adjudicated by
the user on 2026-09-06:

- F1: `chooseEndpoint` prefers a candidate on the agent's own node, so a
  Service with ready endpoints on two or more nodes makes different agents
  choose different endpoints and fight over status. The architecture premise
  is that every agent computes from the same inputs and agrees without
  talking.
- F2: the status contract is not implemented. `NodeStatus.Ready` is hardcoded
  true, `Message` is never set, `Programmed=True` is set from role resolution
  alone with no check that every accepting node has written its rules, and the
  claim-propagation window reports green before the divert/link exist.
- F3: with two or more accepting nodes the code refuses every mapping (only
  the first accepting node by name programs), including when the chosen pod is
  on an accepting node, contradicting the user's chosen semantic.
- F9: mark `0x6b700000` doubles as "slot 0" and "slot unresolved".
- F6 (part): the `corev1.Service` watch in `agent.go:119` has no reader and is
  removed in this task.

User decisions (2026-09-06, recorded in the plan index revision notes):

1. **One serving node, deterministic.** Every agent chooses the same endpoint
   by a global rule (drop the local-wins preference). The mapping is served by
   exactly one accepting node: the one holding the chosen endpoint when that
   node accepts the class, otherwise the first accepting node by name. Every
   other accepting node reports `Programmed=False`,
   `RemotePodMultipleAcceptingNodes`.
2. **F2: implement to the pinned contract** (spec.md conditions, operations.md
   NodeNotReady semantics). Docs already describe the wanted semantics; make
   the code produce them.
3. Fix wave first, then merge.

`docs/spec.md` lines 292-294 and 629-631 still state the old rules; this task
implements the new semantics in code, and task-11 rewrites those docs. Where
this spec and the current spec.md disagree on endpoint/role semantics, THIS
SPEC wins.

## Target

Files you may edit:

- `internal/reconcile/` (all `.go` and `*_test.go`): endpoint choice, role
  decision, status building, slot handling.
- `internal/agent/` (all `.go` and `*_test.go`): status writing, host
  self-check overlay, the Service watch removal in `agent.go`.
- `internal/api/v1alpha1/` ONLY if a field the pinned contract requires is
  missing; if you add or change a field, say so in your report and re-run
  `make verify` (CRD regeneration) plus the CRD copies check.

No RBAC file edits in this task: task-10 owns every change to
`deploy/rbac.yaml` and `chart/templates/rbac.yaml`, including removing the
`services` rule once your watch removal has landed (the wave meta orders the
commits so the rule never disappears while the watch still exists).

Explicit non-goals: no changes under `internal/datapath/` (task-9 owns F7/F10);
no render/golden testdata changes unless a reconcile test proves a golden
changed (report it); no docs/ edits (task-11 owns them); no workflow files
(task-10 owns them).

## Change

Read first: `internal/reconcile/endpoints.go`, `internal/reconcile/status.go`
in full, `internal/reconcile/reconcile.go` (the claim-propagation window,
`slotOrZero`), `internal/agent/` status-writing paths and `agent.go:119`,
`docs/spec.md` sections 190-230 and 255-300 (conditions contract), and the
review report's F1/F2/F3/F9 passages. Establish the current mechanisms before
changing them; the report's line numbers are anchors, not gospel.

### F1 — deterministic endpoint choice and one serving node

Replace the per-node local preference with a rule every agent computes to the
same answer from shared inputs:

1. Gather the ready IPv4 candidates behind the Service (existing
   `gatherCandidates`).
2. Resolve each candidate to the node that runs its pod, through the same
   shared inputs every agent holds (pod index / node addresses as the code
   already does).
3. Choose, in order:
   a. a candidate whose node is an accepting node for the class, picking the
      lexicographically smallest accepting-node name first, then the smallest
      targetRef pod name within that node;
   b. otherwise the candidate with the smallest targetRef pod name (the
      existing fallback).
4. Derive the serving node S: the chosen endpoint's node when that node
   accepts the class, else the lexicographically first accepting node by name
   (when the class selects at least one node).

Role outcomes per agent for the mapping:

- This node is S: program DNAT (existing co-located path when the chosen pod
  is on S; existing remote path with return path when it is remote).
- This node accepts the class but is not S: no rules, `Programmed=False`,
  reason `RemotePodMultipleAcceptingNodes`.
- This node is the chosen pod's node but does not accept: holder role as today
  (reply mark, link claims for the S-pair) — with the F9 change below.
- Otherwise: no contribution.

The mode-None refusal (remote pod with `None` return path) is unchanged but
now anchored on S: every agent computes the same S and the same refusal, so
agents still agree.

Ownership and status: every role and status-ownership decision for a mapping
must derive from the single shared choice (endpoint E and serving node S),
never from a node-local preference. The PortMap status single writer follows
from S; class status rows remain per-node contributions.

Preserve behavior that is already correct: a single accepting node with a
single remote pod (the spec smoke shape), a single accepting node with a local
pod (mode None intranet case), and the class-selects-zero-nodes path.

### F3 — refusal scope

Consequence of F1: the refusal is `RemotePodMultipleAcceptingNodes` for an
accepting node that is not S, whatever the endpoint layout. There is no
"unconditional len(accepting) >= 2 refusal" left; an accepting node that holds
the chosen pod and is S programs its local pod.

### F2 — implement the pinned status contract

Make the code produce what spec.md and operations.md already document:

1. NodeStatus rows carry real readiness. When this agent's node participates
   in a class (accepting or holder), each reconcile pass checks the host:
   every interface the class selects (`class.Spec.Interfaces`) must resolve to
   an address on this node (reuse/extend the agent's interface-address
   resolution; it currently drops unresolvable interfaces silently). Row:
   `Ready: true` when all resolve, else `Ready: false` with `Message` naming
   the first missing interface, in the shape spec.md:225-228 documents
   (`interface wt0 not present`).
2. PortMap `Programmed` honors the pinned conditions table. `True` with
   reason `AllNodesReady` only when every accepting node for the mapping's
   class reports a ready row and S has applied its rules. Otherwise `False`
   with the matching table reason: `NodeNotReady` (an accepting node's row
   missing or not ready, message naming the node and the reason), `NoReadyEndpoint`,
   `ReturnPathUnavailable`, `RemotePodMultipleAcceptingNodes`. The only
   existing `NodeNotReady` use ("class selects zero nodes") keeps its meaning.
3. The claim-propagation window is honest. While a mapping's return-path slot
   is unresolved (the claim has not landed) the code must not report
   `Programmed=True` and must not write rules that depend on the unresolved
   slot: no reply-mark rule (F9), and the status row shows the not-ready state
   until S's rules exist. The level-driven requeue heals it; status is honest
   during the window.
4. The agent's interface check must not silently drop: an unresolvable
   interface is a message, not a skipped row.

### F9 — mark slot ambiguity

`slotOrZero` (reconcile.go, ~line 239) returns 0 for an unresolved slot and
`buildLinkParams`/links.go build a real slot-0 mark from the same value. Fix
by construction: a mapping whose slot is unresolved emits no reply-mark rule
and no link-dependent rules until the claim lands (the F2 change above); the
resolved slot always differs from the unresolved state. Verify no code path
can emit mark `0x6b700000` for an unresolved slot.

### Service watch removal

`internal/agent/agent.go:119` registers `Watches(&corev1.Service{}, toNode)`.
Grep confirms zero readers of Service objects anywhere. Remove the watch from
`SetupWithManager`. Do not touch the RBAC files: task-10 removes the
`services` rule (and narrows the rest) in the same wave, and the wave meta
commits your watch removal before task-10's RBAC hunk lands. The spec's own
watch list (spec.md:256) has five kinds; after removal the code matches it.

## Constraints (reproduced from the plan index; all apply)

1. Toolchain is entered, never assumed. No `go` on the host PATH. Every Go
   command runs as `nix develop -c <cmd>` from `/home/nixos/repos/kuport`, or
   inside a shell entered with `nix develop`.
2. Committing is the wave meta-agent's job. Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag.
3. Append-only git discipline: the branch tip only moves forward. No `git
   reset` (any mode), no `git rebase`, no `git commit --amend`, no history
   reordering, including on unpublished commits.
4. Stay inside your Target. A needed change in another task's file is a stop
   and report, not an edit.
5. `internal/datapath` is the only package that touches the host. No package
   above it opens a netlink socket, shells out, or reads `/proc` — your host
   checks read through the existing agent seam, never new host access in
   `internal/reconcile`.
6. Never shell out to `nft` or `ip`.
7. nftables reserves `mark` and `fwd` as chain names; every kuport chain name
   is prefixed `kup-`.
8. Scratch goes to `/home/nixos/repos/kuport/.tmp/` (gitignored), never
   `/tmp`, never `.cortex/`.
9. Human-facing prose follows `catalyst-v2-writing-docs` (load it before
   writing any doc comment that reads as prose).
10. Acceptance criteria are inviolable. If one cannot be met, stop and report
    it with the criterion intact.
11. Report as a diff, not a commit: files changed, `git diff --stat`, verbatim
    gate output, deviations.
12. Emission discipline: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.
13. Report completion to the wave's meta-agent named in your dispatch brief via
    `c2d steer --agent <meta> "A2A: ..."`, never to the orchestrator.
14. The pinned contracts in the plan index (datapath.State seam, naming and
    numbering, conditions table) are fixed. The conditions table stays exactly
    as pinned: a state that fits no pinned reason is a stop-and-report, never
    a new reason invented on your own.

## Acceptance

Test-first, per `catalyst-v2-sdd-rules` (load it): for each behavior below,
write the failing test first, record the red run, then fix, then green. The
recorded red runs are part of your report.

1. F1 agreement: a table test over reconcile viewpoints (extend the existing
   TestAgentAgreement shape) with ready endpoints on two accepting nodes
   proves every node's computation picks the same endpoint and the same
   serving node S, exactly one node programs, and the other accepting node
   reports `Programmed=False` reason `RemotePodMultipleAcceptingNodes`. Red
   run recorded against current code.
2. F3: a fixture with the chosen pod on a non-first accepting node proves that
   node (as S) programs its local pod and the earlier accepting node is
   refused. Red run recorded.
3. F2 rows: a class whose interface is absent produces the node's row
   `Ready: false` with a message naming the interface; the mapping's
   `Programmed` is `False` with reason `NodeNotReady`. Red run recorded.
4. F2 gate: with every accepting node row ready, `Programmed=True` reason
   `AllNodesReady`; with one accepting node's row not ready, `False`
   `NodeNotReady`.
5. F2/F9 window: a mapping whose return-path claim has not landed emits no
   reply-mark rule and does not report `Programmed=True`; once the slot is
   resolved it does. Red run recorded.
6. Existing suite stays green, plus the new tests:

   ```
   nix develop -c go build ./...
   nix develop -c go vet ./...
   nix develop -c go test ./... -count=1
   nix develop -c golangci-lint run
   ```

   `go test ./...` green includes the reconcile and agent suites with your
   additions. (The envtest tier is build-tagged; task-10 wires it into CI.)

7. Grep proof: no `corev1.Service` reference remains in `internal/agent/`
   (only the import removal). The RBAC `services` rule may still exist in the
   tree at your commit; task-10 removes it.

## Report-to

The wave meta-agent named in your dispatch brief. Report: files changed,
`git diff --stat`, each recorded red run and its green run, gate output
verbatim, deviations. After sending the completion steer, run no further
commands and go idle.
