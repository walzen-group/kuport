# task-9 — Reconciled netlink links and apply ordering (F7, F10)

Execution skill: `catalyst-v2-sdd-rules` for the test-first steps; global
constraints below reproduced from the plan index.

## Context

The final whole-branch review (`.cortex/reports/2026-09-06-whole-branch-final-review.md`)
found two hygiene defects in `internal/datapath`:

- F7: `applyLinks` (netlink.go:44-83) keys only on link name. A `kup-*` link
  that exists is never compared against the desired underlay
  (`Link.LocalAddr` / `RemoteAddr` / `VNI` / `Port`), so a node whose
  InternalIP changes keeps the stale underlay on every existing device and
  traffic stops while status stays green. Only a delete or an agent restart
  heals it.
- F10: `apply.go` commits nft first, then links/rules/routes; a mid-netlink
  failure leaves a half-programmed window that task-8's F2 fix makes visible.
  Ordering link before nft for remote mappings would shrink the window.

## Target

Files you may edit:

- `internal/datapath/netlink.go` (applyLinks and its helpers)
- `internal/datapath/apply.go` (ordering, only for the F10 bounded change)
- `internal/datapath/*_test.go` and `internal/datapath/testdata/` only when a
  test you add proves a golden change is needed (report it)

Explicit non-goals: no changes under `internal/reconcile/`, `internal/agent/`,
`internal/api/` (task-8 owns those; it does not touch datapath files unless a
golden proves otherwise, and it will not). No docs/ edits. No workflow files.

## Change

Read first: `internal/datapath/netlink.go`, `internal/datapath/apply.go`, the
existing apply/link tests, and the review report's F7/F10 passages.

### F7 — existing links are compared against the desired underlay

1. Extract a pure decision function where the shape allows it: given an
   existing link's effective underlay (local address, remote address, VNI,
   port) and the desired `datapath.Link`, decide no-op / update. Unit-test the
   decision table without root: identical params are a no-op, each differing
   param alone forces an update.
2. In `applyLinks`, for a link that exists by name, run the comparison and
   update the device when the underlay differs. Use the netlink library's
   update path (e.g. LinkModify on the Vxlan object carrying the existing
   index/name and the desired attrs); verify against the real kernel the same
   way the existing apply tests do (the repo's netns/unshare harness pattern),
   and confirm the update path works on a live interface in the test if one
   exists in the suite. Where a real-kernel proof is not runnable in this
   environment, name that gap and the experiment that would close it.
3. The update must not tear down the link (address/route churn on the /31
   would drop live traffic); the existing claim/GC design already removes
   links whose name is gone from the desired state.

### F10 — bounded apply-ordering hardening

Attempt the reorder task-8's status honesty makes visible: for a remote
mapping, bring the link up before the nft rules that depend on it, so a
half-programmed node refuses rather than forwards to a pod whose replies
cannot return. Scope: only if the change is small, local to `apply.go`, and
the existing apply/golden tests stay green. If the reorder would ripple into
the reconcile or render seams (it must not), do not force it: report why the
current order stands and rely on task-8's honest status for the window.

## Constraints (reproduced from the plan index; all apply)

1. Toolchain is entered, never assumed. Every command runs as `nix develop -c`
   from `/home/nixos/repos/kuport`, or inside `nix develop`.
2. Committing is the wave meta-agent's job. Leave changes uncommitted; `git
   add` is fine. No commit, no branch, no push, no tag.
3. Append-only git discipline: no `git reset`, `git rebase`,
   `git commit --amend`, or history reordering.
4. Stay inside your Target. A change needed in another task's file is a stop
   and report.
5. `internal/datapath` is the only package that touches the host. Your host
   experiments run inside it, never above it.
6. Never shell out to `nft` or `ip` in code. Test harnesses may use the same
   `unshare`/netns patterns the existing tests use.
7. nftables reserves `mark` and `fwd` as chain names; every kuport chain name
   is prefixed `kup-`.
8. Scratch goes to `/home/nixos/repos/kuport/.tmp/`, never `/tmp`, never
   `.cortex/`.
9. Human-facing prose follows `catalyst-v2-writing-docs` (load it first).
10. Acceptance criteria are inviolable. If one cannot be met, stop and report.
11. Report as a diff, not a commit: files changed, `git diff --stat`, verbatim
    gate output, deviations.
12. Emission discipline: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.
13. Report completion to the wave's meta-agent named in your dispatch brief via
    `c2d steer --agent <meta> "A2A: ..."`, never to the orchestrator.
14. The datapath.State seam and the naming table in the plan index are pinned
    and fixed; the golden rulesets in `testdata/` encode the spec smoke test.

## Acceptance

Test-first, per `catalyst-v2-sdd-rules`: failing test first, recorded red run,
fix, green.

1. F7 decision: a unit test proves an existing link whose underlay differs
   from desired is updated, and one whose underlay matches is left alone. Red
   run recorded against current code (today no comparison exists, so the
   "differs" case must fail red).
2. F7 apply: the netns/kernel-backed link test (or the named gap with its
   experiment) shows a changed local address on an existing `kup-*` link ends
   up with the desired underlay after one apply, without a delete.
3. F10: if the reorder lands, apply tests and golden renders stay green; if it
   does not, the report says why.
4. Suite green:

   ```
   nix develop -c go build ./...
   nix develop -c go vet ./...
   nix develop -c go test ./internal/datapath/... -count=1
   nix develop -c golangci-lint run
   ```

   Run the datapath package tests; do not run project-wide suites. Golden
   tests must pass unchanged unless a red run proves a golden must move, in
   which case report it.

## Report-to

The wave meta-agent named in your dispatch brief. Report: files changed,
`git diff --stat`, each recorded red run and its green run, gate output
verbatim, deviations. After sending the completion steer, run no further
commands and go idle.
