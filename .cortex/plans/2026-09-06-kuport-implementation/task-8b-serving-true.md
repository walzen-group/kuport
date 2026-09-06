# task-8b — Multi-accept mappings report True from the serving node

Execution skill: `catalyst-v2-sdd-rules` for the test-first steps; global
constraints below reproduced from the plan index.

## Context

Wave 4 commit `879f902` implemented the adjudicated single-serving-node
semantics. During the meta hand-back audit, the implementer's flagged
interpretation was put to the user, who decided (2026-09-06):

**A served mapping reports the truth about itself.** A PortMap whose class
selects two or more accepting nodes is served by exactly one node S (the
chosen endpoint's node when it accepts, else the first accepting node by
name). S programs the data plane. The mapping's Programmed condition is True
(reason `AllNodesReady`, meaning redefined in docs by task-11) once the
participating nodes are ready and S's rules exist. Sibling accepting nodes do
not serve this mapping and do not gate its condition. The
`RemotePodMultipleAcceptingNodes` refusal retires from the mapping surface:
single-serving prevents the several-nodes-forwarding-one-pod conflict
structurally, so no refusal case remains. `Published` narrows to S: the only
addresses where the port answers.

Current code (the reading being replaced): `decideRole`
(internal/reconcile/endpoints.go:172-177) sets `Programmed=False` with reason
`RemotePodMultipleAcceptingNodes` for every agent, S included, whenever
`len(m.acceptingNodes) >= 2`, so a multi-accept mapping can never read True.
`Published` rows still list every accepting node x interface.

## Target

Files you may edit:

- `internal/reconcile/endpoints.go` (decideRole; any now-dead helpers)
- `internal/reconcile/reconcile.go` (resolveProgrammed / acceptingRowsReady
  participant set)
- `internal/reconcile/status.go` (publishedRows: S only)
- `internal/api/v1alpha1/` (remove `ReasonRemotePodMultipleAcceptingNodes`
  after grepping every reference; it is a pre-release API, removal is clean
  if nothing else uses it)
- `internal/reconcile/*_test.go` (update flipped expectations; add the red-run
  test)
- `deploy/rbac.yaml`, `chart/templates/rbac.yaml`, `.github/workflows/`,
  `docs/`, `chart/` prose: NOT yours (task-11 owns docs; no RBAC/workflow
  change is expected)

## Change

Read first: `internal/reconcile/endpoints.go`, `reconcile.go`,
`status.go`, and the wave-4 hand-backs at
`.cortex/reports/handbacks/task-8-semantics.md` (deviations 3 and 6) and
`wave-4-gate-evidence.md`.

### Serving-node Programmed

Remove the `len(m.acceptingNodes) >= 2` early-False branch from decideRole so
every mapping with an endpoint and a serving node proceeds to the gated path.
The gate (resolveProgrammed) then settles True for a served mapping when the
participants are ready and the rules can exist:

- The serving node S's class node row is ready (its class interfaces
  resolve; the row mechanism from task-8 stays).
- When the chosen pod is remote (S is not the pod's node): the holder node
  (the pod's node) row is ready and the return-path slot for the S-holder
  pair has landed, so S's link and the holder's reply-mark can exist. The
  claim-propagation window reports honestly (not True) until then, as
  task-8 built it.

Sibling accepting nodes are outside the mapping: their rows do not gate it and
they write no mapping status. S remains the single status writer.

Consequences to implement:

- `acceptingRowsReady` (or its replacement) checks the participants above,
  never the full accepting-node set, for the Programmed gate. The class
  status node rows keep every accepting node's row (a per-node readiness
  fact) unchanged.
- Reason `RemotePodMultipleAcceptingNodes`: grep every reference (code,
  tests, api constants). Remove the constant and each reference once the
  decideRole branch is gone. If a reference survives with a real meaning you
  can defend, stop and report it rather than keeping dead API.
- No new condition reason is invented; True keeps reason `AllNodesReady` and
  task-11 redefines its documented meaning. False keeps the existing reasons
  (`NoReadyEndpoint`, `ReturnPathUnavailable`, `NodeNotReady` with a message
  naming the unready participant).

### Published narrows to S

`publishedRows` (or the code that fills Published addresses on the PortMap
status) lists only the serving node S's interfaces and resolved addresses:
the addresses where DNAT rules exist. Non-serving accepting nodes appear
nowhere in Published.

## Constraints (reproduced from the plan index; all apply)

1. Toolchain is entered, never assumed. Commands run as `nix develop -c` from
   `/home/nixos/repos/kuport`.
2. Committing is the wave meta-agent's job. Leave changes uncommitted; `git
   add` is fine. No commit, no branch, no push, no tag.
3. Append-only git discipline: no `git reset`, `git rebase`,
   `git commit --amend`, or history reordering.
4. Stay inside your Target. A change needed in another task's file is a stop
   and report.
5. `internal/datapath` is the only package that touches the host; you touch no
   host code.
6. Never shell out to `nft` or `ip`.
7. nftables chain names are `kup-`-prefixed.
8. Scratch goes to `/home/nixos/repos/kuport/.tmp/`, never `/tmp`, never
   `.cortex/`.
9. Human-facing prose follows `catalyst-v2-writing-docs` (load it before doc
   comments that read as prose).
10. Acceptance criteria are inviolable. If one cannot be met, stop and report.
11. Report as a diff, not a commit: files changed, `git diff --stat`, verbatim
    gate output, deviations.
12. Emission discipline: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.
13. Report completion to the wave meta-agent named in your dispatch brief via
    `c2d steer --agent <meta> "A2A: ..."`, never to the orchestrator.
14. The plan index conditions table is amended by this adjudication
    (RPMAN removed from the PortMap Programmed false reasons); the pinned
    reasons that remain are unchanged.

## Acceptance

Test-first per `catalyst-v2-sdd-rules`; record the red run against the
current code (879f902) before the fix.

1. Multi-accept world reaches True: a table test (extend or adapt the wave-4
   `TestAgentAgreementMultiAccepting` world) proves that with two accepting
   nodes, ready participant rows and a landed slot (local pod on S: no slot
   needed), every agent's computation agrees on E and S and the mapping reads
   `Programmed=True` reason `AllNodesReady` from S. Red run: against current
   code this reads False/RPMAN.
2. Remote multi-accept gates honestly: chosen pod on a non-accepting node,
   class selects two accepting nodes; before the S-holder claim lands the
   mapping reads not-True (NodeNotReady or ReturnPathUnavailable as the
   window reason task-8 chose), after it lands True.
3. Sibling non-participation: an accepting sibling's row not ready does not
   block S's mapping from True when S's row is ready (local pod case).
4. Published: the mapping's Published addresses are exactly S's resolved
   interfaces; no sibling node appears. Red run: current code lists every
   accepting node.
5. RPMAN gone: grep `RemotePodMultipleAcceptingNodes` and
   `MultipleAccepting` repo-wide returns nothing outside `.cortex/` and
   `.tmp/`.
6. Suite green:

   ```
   nix develop -c go build ./...
   nix develop -c go vet ./...
   nix develop -c go test ./... -count=1
   nix develop -c golangci-lint run
   ```

   `go test ./...` includes the updated reconcile suite; no test is weakened
   to pass (a flipped expectation names the adjudicated semantics in its
   comment).

## Report-to

The wave meta-agent named in your dispatch brief. Report: files changed,
`git diff --stat`, the recorded red run and its green run, gate output
verbatim, deviations. After sending the completion steer, run no further
commands and go idle.
