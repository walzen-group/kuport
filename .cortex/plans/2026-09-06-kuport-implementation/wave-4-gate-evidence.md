# wave 4 gate evidence — meta verification record

Verification by meta-wave4. Each block: worker's recorded gates (not re-run
by meta), the meta's diff-vs-spec read, and meta decisions on deviations.

## task-10 — fix-deploy-ci (F5, F6, F8, OCI) — VERIFIED 2026-09-06 13:2x

Worker report: `.cortex/reports/handbacks/task-10-fix-deploy-ci.md`.
Diff: `.tmp/task10/my.diff`; red-run render: `.tmp/task10/render-before.yaml`.

Changes landed (uncommitted, verified against spec by direct diff read):

- `chart/templates/_helpers.tpl` — fallback exactly the pinned
  `default (printf "v%s" .Chart.AppVersion)` form from the spec.
- `chart/values.yaml`, `chart/README.md` — v-prefixed default documented;
  checkout-install caveat + OCI pull sentence present.
- `deploy/rbac.yaml` + `chart/templates/rbac.yaml` — `["list","watch"]` on
  portmapclasses, portmaps, endpointslices, nodes, namespaces;
  `["update"]` on both status subresources; `services` rule removed from both
  (grep count 0 in working tree); resourceNames-scoped configmaps `get` kept.
- `.github/workflows/ci.yaml` — new `envtest` job between generated and image
  jobs; same runner pattern as siblings.
- `.github/workflows/release.yaml` — push step after Package step;
  `helm push "kuport-$VERSION.tgz" oci://ghcr.io/walzen-group/kuport`;
  `VERSION` reaches the step via `$GITHUB_ENV` (release.yaml:135-136), the
  same scope the render/CRD steps use; credentials via env per file pattern.

Recorded gates (worker): red first — render `:0.1.0`, old verbs/services
rules present, `grep -c envtest` = 0, `grep -c 'helm push'` = 0. Green —
`:v0.1.0`, scratch `:v1.2.3`, tag/digest precedence intact, `helm lint` 0
failed, envtest `ok github.com/walzen-group/kuport/internal/agent 13.622s`
(assets 1.37.0, pristine-HEAD export), actionlint 1.7.12 + shellcheck 0.11.0
clean, `helm package` to `.tmp/` OK.

Deviations accepted (4): (1) literal acceptance-2 `kubectl apply
--dry-run=client -f deploy/rbac.yaml -f chart/templates/` cannot pass as
written (same discovery trap recorded for CI run 34031346074; kubectl also
cannot parse unrendered templates) — replaced by kubectl-validate, the
repo's own offline validator; (2) F8 asset fetch moved out of the prefix
assignment so a failed fetch aborts instead of self-skipping green — provably
strengthens the acceptance intent; (3) envtest gate run on HEAD export
because fix-linkapply's in-progress datapath edits didn't compile at check
time (mid-wave shared-tree condition, expected); (4) real ghcr push not
attempted, named per spec.

Meta note: worker's report calls the in-flight `internal/datapath` change
"task-8's"; it is task-9's (fix-linkapply). Label only, no contract impact.

Commit hold: RBAC files must land only with or after task-8's
`agent.go:119` watch removal (HEAD e5ef0c8 still carries the watch).

## task-9 — fix-linkapply (F7, F10) — VERIFIED 2026-09-06

Worker report: `.cortex/plans/2026-09-06-kuport-implementation/task-9-gate-evidence.md`.
Transcripts: `.tmp/f7-red-run.txt`, `.tmp/f7-green-run.txt`,
`.tmp/f7-kernel-run.txt`, `.tmp/task9-gates.txt`, probe logs
`.tmp/f7-probe-out.txt`, `.tmp/f7-ippattern-probe.txt`.

Changes landed (verified by direct read of the full production diff):

- `internal/datapath/netlink.go` — `underlayOf` extraction, pure
  `linkActionFor` decision (keep/retarget/rebuild), `applyLinks` branches,
  `realNetlink.LinkSetUnderlay` (minimal RTM_NEWLINK with
  IFLA_VXLAN_LOCAL/GROUP). The /31 address and routes are restated in the
  same pass; retarget keeps device index and liveness.
- `internal/datapath/apply.go` — F10: `applyLinks` before `applyNFT`;
  rules/routes stay last (they resolve device indexes). `underlaySetter` is
  an optional unexported capability; `NetlinkConn` and `datapath.State`
  (pinned seams) untouched.
- Tests: `netlink_test.go` (decision table, stale-underlay update, no-churn
  pin, setter-absence convergence), `netlink_kernel_test.go` (real-kernel
  netns proof, skip-guarded), `apply_test.go` fake gains `LinkSetUnderlay`.

Recorded gates (worker): red first — stale-underlay and per-param cases fail
because no comparison exists at HEAD; the no-churn companion passed pre-fix.
Green — all 17 package tests, incl. `TestApplyLinksUpdatesUnderlayOnRealKernel`
on 6.18.33.2-WSL2 (index kept, link UP, /31 intact, fresh dump re-reads as
keep — no update loop). Scoped gates: build, vet, package test, package
golangci-lint 0 issues. Goldens unchanged.

Deviations accepted (4): LinkModify proven impossible on vishvananda v1.3.1
(always re-sends attrs the kernel rejects by presence; probe + kernel-source
grounding); three-way decision because VNI/port cannot change on a live
device at all; optional-interface seam chosen to avoid editing the agent's
committed fake (constraint-4-correct); whole-repo vet failures at check time
were task-8's mid-flight test files, not task-9's. Spec-premise note: the
committed suite had no netns harness; it now does.

Committed: `fe0b3f2`.

## task-8 — fix-semantics (F1, F2, F3, F9, watch removal) — VERIFIED 2026-09-06

Worker report: `.cortex/reports/handbacks/task-8-semantics.md`.
Full diff: `.tmp/task8/my.diff`. Red/green/gate logs: `.tmp/task8-reds/`
(red-phase1-behavioral.log, red-phase2-interface-check.log,
green-phase-tests.log, gates-final.log).

Changes landed (verified by direct read of the full production diff):

- `internal/reconcile/endpoints.go` — `chooseEndpoint` ranks by accepting
  node (name-ordered), then targetRef pod name, then node, then addr: no
  local-wins, no slice-order dependence. `decideRole` derives serving node S
  (endpoint node when accepting, else `acceptingNodes[0]`), sets
  `effAccepting = S`, refusal precedence None > RPMAN kept, single-accepting
  case gated for post-allocation resolution.
- `internal/reconcile/reconcile.go` — `Inputs.InterfaceAddrs` (agent host
  read); `resolveProgrammed`/`acceptingRowsReady` (F2.2: True only when
  every accepting row ready + landed slot + buildable link params);
  `slotOrZero` deleted; mark/link/divert/route all behind `landedSlot`
  (F9 by construction — mark only exists inside `buildLinkParams`, whose
  callers are guarded).
- `internal/reconcile/links.go` — `slotFor` -> `landedSlot`: only landed
  claims build host objects; `newly` proposes claims only.
- `internal/reconcile/status.go` — `nodeRowFor` real readiness with
  `interface X not present` message (spec.md:225-228 shape); `statusOwner`
  follows S.
- `internal/agent/` — Service watch removed (acceptance 7 grep proof
  re-verified by meta: zero `corev1.Service` in internal/agent);
  `resolveInterfaces` through the existing Host seam (constraint 5 holds);
  `writeClass` passes Compute's row through, MTUs stamped only.

Recorded gates (worker): red phase-1 — four behaviors fail against pre-fix
code with real defect output (incl. `Mark:1802502144` = the 0x6b700000
defect emitted during the window); red phase-2 — F2.1 red by mechanism
absence. Green — five new tests + updated suite; full gates exit=0 x4
verbatim (build/vet/test/lint) at the composite tree. Meta independently
re-verified the grep proofs and spot-checked the five flipped existing
tests: they pin strict assertions (exact ownership writer, exact message
shape), none weakened.

Deviations flagged (6; see report): five existing tests updated to the
adjudicated semantics; deviation 3 is an interpretation for orchestrator
review — with >1 accepting node, every agent incl. S computes
False/RPMAN and S (single writer) writes it, so such a mapping never reads
True while the class selects >1 node. Consistent with the single-writer
rule and decision 1's text; spec acceptance 1 only requires the other
accepting node to compute the refusal. task-11 docs should state it.
Deviation 6: `Published` still lists every accepting node; whether it
narrows to S is a task-11/follow-up call.

Committed: `879f902` (first of three, per the watch-before-RBAC ordering).

## Whole-change verification — meta-run at merged tip 8fea956

```
nix develop -c go build ./...                      exit=0
nix develop -c go vet ./...                        exit=0
nix develop -c go test ./... -count=1              all ok (agent 0.187s,
                                                   datapath 0.652s,
                                                   reconcile 0.009s) exit=0
nix develop -c golangci-lint run                   0 issues, exit=0
nix develop -c make verify                         no CRD drift, exit=0
nix develop -c helm lint chart/                    0 failed, exit=0
envtest (KUBEBUILDER_ASSETS=setup-envtest 1.37.0)  ok internal/agent 12.314s
```

The envtest tier was re-run by meta on the final tree (task-10's own gate
ran at pristine HEAD; task-8 since changed the agent package). Green.

Commits: `879f902` (task-8) -> `fe0b3f2` (task-9) -> `8fea956` (task-10),
branch `feat/initial-implementation`, 3 ahead of origin. Push is the
orchestrator's action; CI re-run on the pushed tip decides the merge gate
together with wave 5 docs (task-11).

## Behavioral notes on the workers (routine, no incidents filed)

- fix-linkapply: after its correct completion push, it violated the spec's
  Report-to rule by launching a background `herdr agent wait meta-wave4`
  — holding its turn open on a wake condition it was never owed, which read
  as in-flight and blocked the hand-back gate. Corrected by A2A steer at
  13:5x: cancel the wait, go idle. Settled after steering.
- task-8 deviation 3 (multi-accepting never reads True even on S) is an
  interpretation the orchestrator should confirm or overrule at audit;
  task-11 docs must state whichever reading stands.
