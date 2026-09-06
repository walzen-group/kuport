# task-11-docs — worker completion report (as delivered to meta-wave5)

Recorded 2026-09-06 by meta-wave5 from the worker's `A2A:` steer. Worker:
fix-docs (omp, cwd /home/nixos/repos/kuport, spec
`.cortex/plans/2026-09-06-kuport-implementation/task-11-docs.md`). Changes
were left uncommitted per wave discipline; the meta committed them as
3597700.

---

task-11-docs complete. All changes uncommitted in the working tree for you to commit.

FILES CHANGED (git diff --stat):

```
 README.md                               |   8 +-
 chart/crds/kuport.wlz.li_portmaps.yaml  |   5 +-  (regen)
 config/crd/kuport.wlz.li_portmaps.yaml  |   5 +-  (regen)
 deploy/crds/kuport.wlz.li_portmaps.yaml |   5 +-  (regen)
 docs/integration.md                     |  51 +++---
 docs/operations.md                      |  46 ++---
 docs/spec.md                            | 151 +++++++++++------
 internal/api/v1alpha1/portmap_types.go  |   5 +-
 8 files changed, 163 insertions(+), 113 deletions(-)
```

COVERAGE of the nine change items: (1) endpoint choice rewritten to the
global lexicographic rule in spec.md The reconcile; (2) single-serving rule
in spec.md class table, refuse section, operations.md NodeNotReady row,
integration.md intranet rationale; (3) status contract: node-row
ready/message semantics, AllNodesReady gate reworded to
serving-row-plus-landed-slot, claim-propagation window paragraph in spec
Status writing, honest ReturnPathUnavailable wording in ops; (4) CI
sentences now true (spec.md ci.yaml paragraph gained the envtest job;
intro/Testing/README/integration say CI runs the tiers); (5) chart default
v&lt;appVersion&gt; stated in spec release paragraph and integration.md, OCI chart
reference added to spec, README and integration install steps; (6)
RemotePodMultipleAcceptingNodes deleted everywhere (spec paragraph, ops
row, integration bullet rewritten to single-serving); (7) Programmed True
meaning rewritten to S-rules-plus-participants gate, holder gate described
as landed slot plus both link addresses; (8) Published = serving node rows
in spec, ops, and the portmap_types.go comment; CRDs regenerated:
config/crd == deploy/crds == chart/crds; (9) sweep done:
accepting-node-as-forwarder renamed serving node through datapath
headings/participants, statusOwner chain updated in ops, reschedule row
updated.

GATE EVIDENCE:

- `nix develop -c make verify`: clean (diff -ru config/crd .tmp/crd silent),
  run again after the final edits.
- Acceptance grep over docs/ + README.md + internal/api for: candidate on
  this node | RemotePodMultipleAcceptingNodes | remote pod on multiple | pod
  on every accepting | one row per accepting | every accepting node has |
  holding the chosen endpoint writes | first accepting node programs, zero
  hits. Remaining "first accepting node" hits are only the serving rule and
  the statusOwner fallback chain, both conditioned on the new rule.
- `git status --short`: only the eight files above, plus untracked .cortex/
  and .tmp/; nothing staged.
- Humanizer pass run on all touched prose: no em dashes, no curly quotes, no
  banned words, no rule-of-three padding.

DEVIATIONS (outside the worker's Target, left to the meta):

1. `internal/api/v1alpha1/portmapclass_types.go:15` Interfaces field comment
   still says "per accepting node. One rule per interface per mapping." Same
   staleness fixed on Published; editing another field comment is out of the
   worker's scope.
2. `internal/agent/status.go` fillPublished doc comment still describes
   resolving rows for another accepting node from its class report; that
   path is now unreachable (every published row names the writing node).
3. No image-tag example in the four prose files needed retargeting to
   v0.1.0; item 5 was satisfied via the OCI reference plus the
   source-checkout caveat instead.

Going idle now.
