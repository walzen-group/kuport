# task-11 — Docs aligned to the adjudicated semantics (F1/F2/F3 docs, F8 claim, F5)

Execution skill: `catalyst-v2-writing-docs` (REQUIRED: load it first; its
mandatory humanizer pass applies to every file you touch). Global constraints
below reproduced from the plan index.

## Context

Wave A (tasks 8-10) and wave 4.1 (commit 8d68d2c, task-8b) implemented the
user's 2026-09-06 adjudications in code. The tree you verify docs against is
the 8d68d2c tip.

- One serving node, deterministic: every agent chooses the same endpoint by a
  global rule (candidate on the lexicographically first accepting node, else
  smallest pod name); the mapping is served by exactly one accepting node
  (the chosen pod's node when it accepts, else the first accepting node by
  name); every other accepting node reports `Programmed=False`,
  `RemotePodMultipleAcceptingNodes`.
- The pinned status contract is implemented: NodeStatus rows carry real
  readiness and messages; `Programmed=True` (reason `AllNodesReady`) only when
  every accepting node reports ready and the serving node applied its rules;
  the claim-propagation window reports honestly.
- The Service watch is gone and RBAC matches actual calls; the chart's default
  image tag is `v<appVersion>`; CI runs the envtest tier; release.yaml pushes
  the chart to `oci://ghcr.io/walzen-group/kuport`.
- Wave 4.1 (second adjudication): a served mapping reports Programmed=True
  from its serving node; RemotePodMultipleAcceptingNodes is deleted from the
  API and unreachable; Published lists only the serving node.

Your job: make the docs state what the code now does. Where this spec and a
doc disagree, THIS SPEC wins until your edit lands; after your edit the code
is authoritative and you verified it.

## Target

Files you may edit (all of them human-facing prose):

- `docs/spec.md`
- `docs/operations.md`
- `docs/integration.md`
- `README.md` (root)
- `internal/api/v1alpha1/portmap_types.go`: the Published field's doc comment
  ONLY (it still says one row per accepting node; Published now lists the
  serving node). After the edit, regenerate the CRD copies with
  `nix develop -c make verify` (config/crd, deploy/crds, chart/crds must be
  byte-identical to the generated output).

Explicit non-goals: no Go behavior changes, no YAML behavior changes, no
workflow edits, no `chart/README.md` (task-10 owns it), no `docs/` file beyond
the three named, no other field comment edits.
If you find a doc statement that contradicts code and is not in the list
below, fix it too; that is the task's purpose.

## Change

Read the code first: `internal/reconcile/endpoints.go`, `status.go`,
`reconcile.go`, `internal/agent/` status paths, plus the three docs and root
README in full. Then hunt every passage asserting the old rules and rewrite:

1. **Endpoint choice** (spec.md ~292-294 and anywhere else the per-node
   "candidate on this node wins" rule appears): replace with the global
   deterministic rule, stated as: candidates are resolved to their nodes from
   shared inputs; the chosen endpoint is the one on the lexicographically
   first accepting node that holds a ready candidate (pod name as tiebreak),
   else the ready candidate with the smallest pod name; every agent computes
   the same answer, which is what makes the agents agree without talking.
2. **Multi-accept and the serving node** (spec.md ~629-631 internal-class
   pattern, operations.md ~81 refusal scope, and anywhere the old
   "first accepting node by name programs regardless of endpoint location"
   reading appears): replace with the single-serving rule — a mapping with
   several accepting nodes is served by exactly one: the chosen pod's node
   when it accepts, else the first accepting node by name; the other accepting
   nodes report `Programmed=False` reason `RemotePodMultipleAcceptingNodes`.
   The internal-class pattern "a pod on every accepting node" is dropped as a
   sanctioned multi-node pattern; a class may still select several nodes, but
   one serves.
3. **Status contract** (spec.md 195-196, 225-228 and operations.md ~80):
   confirm the docs describe what the code now produces: node rows with real
   readiness and messages (`ready: false, message: interface wt0 not present`),
   `Programmed=True` reason `AllNodesReady` only when every accepting node is
   ready, `NodeNotReady` with a message otherwise, honest reporting during the
   claim-propagation window. Adjust any passage that promised more or less.
4. **CI tiers** (spec.md ~788 and ~7): the claim that GitHub Actions runs the
   unit, golden and envtest tiers is now true; keep the sentence, adjust if
   its wording described the old gap.
5. **Chart image default** (root README and spec install examples that pull or
   reference image tags): the chart default is `v<appVersion>`; a source
   checkout install needs a published release or an explicit tag/digest. Make
   examples use a tag that a release actually pushes (`v0.1.0`), a digest, or
   the OCI chart reference.
6. **RemotePodMultipleAcceptingNodes is gone**: every doc row that names it as
   a PortMap Programmed false reason (spec.md ~196 and ~201, operations.md
   ~81, integration.md ~326, and any other passage describing the refusal or
   the old first-accepting-node-by-name programming rule) is removed or
   rewritten to the single-serving model: one node S serves the mapping;
   siblings are outside it.
7. **Programmed True meaning** (spec.md ~195): rewrite so True (reason
   AllNodesReady) means the serving node S has written its rules and every
   required participant is ready. For a remote chosen pod the required
   participant gate is the landed S-holder return-path slot plus both link
   addresses; the holder (a non-accepting node) writes no class node row, so
   the docs describe the gate that exists, not a row gate that cannot open.
8. **Published** (spec.md and anywhere rows-per-accepting-node appear): the
   mapping's Published lists the serving node's interfaces and resolved
   addresses only, where the DNAT rules exist. Fix the portmap_types.go
   comment accordingly and regenerate.
9. **Consistency sweep**: any other passage describing local-wins, the
   unconditional multi-accept refusal, per-node Programmed green, the Service
   watch count (spec.md ~256), RBAC broader than the code's calls, or
   Published listing every accepting node.

The spec.md smoke fixture and golden descriptions must still describe the
implemented render; verify, do not rewrite what is correct.

## Constraints (reproduced from the plan index; all apply)

1. Toolchain is entered, never assumed. Commands run as `nix develop -c` from
   `/home/nixos/repos/kuport` when you need one.
2. Committing is the wave meta-agent's job. Leave changes uncommitted; `git
   add` is fine. No commit, no branch, no push, no tag.
3. Append-only git discipline: no `git reset`, `git rebase`,
   `git commit --amend`, or history reordering.
4. Stay inside your Target.
5. You touch no code and no host state.
6. Never shell out to `nft` or `ip`.
7. nftables chain names are `kup-`-prefixed.
8. Scratch goes to `/home/nixos/repos/kuport/.tmp/`, never `/tmp`, never
   `.cortex/`.
9. Human-facing prose follows `catalyst-v2-writing-docs` and its mandatory
   humanizer pass; this is the whole task.
10. Acceptance criteria are inviolable. If one cannot be met, stop and report.
11. Report as a diff, not a commit: files changed, `git diff --stat`, gate
    output, deviations.
12. Emission discipline: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.
13. Report completion to the wave's meta-agent named in your dispatch brief via
    `c2d steer --agent <meta> "A2A: ..."`, never to the orchestrator.

## Acceptance

1. Grep the four files for the old-rule vocabulary and prove it is gone:
   "candidate on this node wins", "RemotePodMultipleAcceptingNodes" (and the
   phrase "remote pod on multiple accepting nodes" in any form), any
   unconditional "first accepting node" serving statement that ignores where
   the chosen pod runs, "pod on every accepting node" as a working pattern
   (if it remains, it is a description of why a class selects several nodes
   while one serves — read the sentence in context).
2. Each rewritten passage states the code's actual rule (verified against
   `internal/reconcile/endpoints.go` and `status.go` at the commit you sit
   on).
3. Prose passes the humanizer pass: no em dashes, no "X, not Y"
   constructions, no banned words, structure per `catalyst-v2-writing-docs`.
4. `nix develop -c make verify` is clean after the portmap_types.go comment
   edit (the CRD copies regenerate byte-identical).
5. Nothing outside your named files changed: `git status --short` shows only
   them (plus the `.cortex/` tree and anything the previous wave left — do not
   stage or commit anything; report the working-tree state).

## Report-to

The wave meta-agent named in your dispatch brief. Report: files changed,
`git diff --stat`, the grep evidence, deviations. After sending the completion
steer, run no further commands and go idle.
