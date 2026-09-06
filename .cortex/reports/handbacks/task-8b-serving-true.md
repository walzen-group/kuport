# task-8b handback — fix-serving-status -> meta-wave41

The adjudicated serving-node semantics (user word, 2026-09-06: a served
mapping reports the truth about itself) are implemented, uncommitted.
Test-first per `catalyst-v2-sdd-rules`: red run recorded against the
current tree (879f902 semantics + task-9/10, none of which touched these
paths) before the fix; green after. Gates green verbatim below.

## Files changed

| File | Change |
| --- | --- |
| internal/reconcile/endpoints.go | decideRole: removed the `len(m.acceptingNodes) >= 2` early-False/RPMAN branch; every accepted mapping with an endpoint and an accepting node now sets `gated` (S stays sole effAccepting writer). `fmt` import dropped (branch was its only user); doc comments reworded to the adjudicated reading. |
| internal/reconcile/reconcile.go | `acceptingRowsReady` -> `servingRowReady`: the row gate checks S's class node row only — never the full accepting set; sibling rows stay per-node readiness facts. resolveProgrammed/Compute/mapping.gated comments updated. Remote gate unchanged in mechanism: landed S-holder slot + buildLinkParams (task-8's honest window). |
| internal/reconcile/status.go | publishedRows: S's interfaces only; nil when no serving node. Doc updated (addresses still filled by the agent's fillPublished). |
| internal/api/v1alpha1/conditions.go | `ReasonRemotePodMultipleAcceptingNodes` constant removed (only users were decideRole and the flipped tests). ConditionProgrammed comment: "every accepting node has written its rules" -> "the serving node has written its rules". |
| internal/reconcile/reconcile_test.go | Red-run test `TestMultiAcceptRemotePodWindow` (new); flipped `TestRefusals/two accepting nodes with a remote pod` (False/RPMAN -> False/NodeNotReady — no rows seeded, honest gate), `TestAgentAgreementMultiAccepting` (+ `multiAcceptingWorld` seeded with S-ready + sibling-not-ready rows -> True/AllNodesReady from S and at every viewpoint; sibling not gating == acceptance 3), `TestServingNodeIsChosenPodsNode` (S=row-seeded node-b -> True). Each flipped expectation names the adjudication in its comment. |
| internal/reconcile/status_test.go | `TestPublishedRows` flipped: exactly S's two rows; sibling node-b appears nowhere (acceptance 4). |
| internal/reconcile/helpers_test.go | `resolvedMappingFor` now runs the allocation + resolveProgrammed passes as Compute orders them, so the agreement loop compares settled conditions, not pre-gate empties. |

## git diff --stat

```
 internal/api/v1alpha1/conditions.go  |  11 ++--
 internal/reconcile/endpoints.go      |  27 ++++----
 internal/reconcile/helpers_test.go   |  17 +++--
 internal/reconcile/reconcile.go      |  48 +++++++-------
 internal/reconcile/reconcile_test.go | 124 ++++++++++++++++++++++++++---------
 internal/reconcile/status.go         |  33 +++++-----
 internal/reconcile/status_test.go    |  11 ++--
 7 files changed, 168 insertions(+), 103 deletions(-)
```

## Red run (verbatim, .tmp/task8b/red.log)

Recorded against the pre-fix tree (HEAD 8fea956; decideRole/gate/Published
code identical to `879f902`), full reconcile package, only these five
behaviors fail:

```
--- FAIL: TestRefusals/two_accepting_nodes_with_a_remote_pod
        reconcile_test.go:146: Programmed = False/RemotePodMultipleAcceptingNodes, want False/NodeNotReady while node-a has not reported
--- FAIL: TestAgentAgreementMultiAccepting
        reconcile_test.go:303: node-a Programmed = False/RemotePodMultipleAcceptingNodes, want True/AllNodesReady (a served mapping reports the truth about itself)
        reconcile_test.go:324: world(node-a): Programmed = False/RemotePodMultipleAcceptingNodes, want True/AllNodesReady
        reconcile_test.go:324: world(node-b): Programmed = False/RemotePodMultipleAcceptingNodes, want True/AllNodesReady
        reconcile_test.go:324: world(node-c): Programmed = False/RemotePodMultipleAcceptingNodes, want True/AllNodesReady
--- FAIL: TestServingNodeIsChosenPodsNode
        reconcile_test.go:380: node-b Programmed = False/RemotePodMultipleAcceptingNodes, want True/AllNodesReady (a served mapping reports the truth about itself)
--- FAIL: TestMultiAcceptRemotePodWindow/unlanded_claim
        reconcile_test.go:419: window Programmed = False/RemotePodMultipleAcceptingNodes, want False/ReturnPathUnavailable
--- FAIL: TestMultiAcceptRemotePodWindow/landed_claim
        reconcile_test.go:428: after landing Programmed = False/RemotePodMultipleAcceptingNodes, want True/AllNodesReady
--- FAIL: TestPublishedRows
        status_test.go:84: published rows = 4, want 2: [{Node:node-a Interface:eth0 ...} {Node:node-a Interface:eth1 ...} {Node:node-b Interface:eth0 ...} {Node:node-b Interface:eth1 ...}]
```

## Green run + gates (verbatim, .tmp/task8b/green-reconcile.log, .tmp/task8b/gates.log, .tmp/task8b/lint.log)

```
$ nix develop -c go test ./internal/reconcile/ -count=1
ok  	github.com/walzen-group/kuport/internal/reconcile	0.008s
$ nix develop -c go build ./...          exit=0
$ nix develop -c go vet ./...            exit=0
$ nix develop -c go test ./... -count=1
?   	github.com/walzen-group/kuport/cmd/kuport-agent	[no test files]
ok  	github.com/walzen-group/kuport/internal/agent	0.194s
?   	github.com/walzen-group/kuport/internal/api/v1alpha1	[no test files]
ok  	github.com/walzen-group/kuport/internal/datapath	0.665s
ok  	github.com/walzen-group/kuport/internal/reconcile	0.011s
exit=0
$ nix develop -c golangci-lint run       0 issues. exit=0
$ nix develop -c gofmt -l internal/      (empty)
```

## Acceptance walk

1. Multi-accept True: `TestAgentAgreementMultiAccepting` on the extended
   `multiAcceptingWorld` (ready S row, local pod on S): every viewpoint
   agrees E=10.244.7.7, S=node-a; True/AllNodesReady written only by S.
   Red recorded.
2. Remote multi-accept window: `TestMultiAcceptRemotePodWindow` —
   unlanded False/ReturnPathUnavailable, landed True/AllNodesReady with
   DNAT+link. Red recorded.
3. Sibling non-participation: same world seeds node-b (sibling) with
   Ready:false; node-a still reads True. Red recorded.
4. Published exactly S: `TestPublishedRows` flipped to node-a eth0/eth1
   only. Red recorded (got 4 rows listing the sibling).
5. RPMAN gone: `git grep -n "RemotePodMultipleAcceptingNodes|MultipleAccepting"
   -- ':!.cortex' ':!.tmp'` returns only the four docs/ rows (see
   deviation A). Code surface (`internal/`, `cmd/`, `config/`, `deploy/`,
   `chart/`, Makefile, flake.nix) is clean, as is the dead helper name
   `acceptingRowsReady`.
6. Suite green: verbatim above; no test weakened — flips name the
   adjudication in their comments.

## Deviations / decisions

A. Acceptance 5 literal ("nothing outside .cortex/ and .tmp/") collides
   with the Target ("docs/ prose: NOT yours — task-11 owns docs") because
   docs/integration.md:326, docs/operations.md:81, docs/spec.md:196,201
   still carry the reason. I did not edit docs; the code half of the
   criterion is met with evidence above. The meta should confirm task-11
   picks up those four lines plus the plan-index conditions-table row
   (constraint 14's amendment) — I left .cortex/ untouched.
B. Spec line "the holder node (the pod's node) row is ready" is
   implemented through the existing landed-slot + buildLinkParams
   mechanism, not a literal class-row check: a remote mapping's holder is
   by construction non-accepting (a remote S means the pod's node does
   not accept), and only accepting nodes ever write class rows
   (status.go buildClassStatus; agent writeClass guards on
   `contrib.Node.Name != ""`). A literal holder-row gate could never open
   and would hang every remote mapping and flip task-8's green
   TestClaimWindowReportsHonest. The intent — S's link and the holder's
   reply-mark can exist — is exactly what the landed slot + both
   addresses guarantee.
C. portmap_types.go Published field comment still reads "one row per
   accepting node and interface"; it is embedded verbatim in
   config/crd, chart/crds and deploy/crds descriptions, and those files
   are outside my Target (regeneration would touch chart/). Left for
   task-11 or a manifest-regeneration follow-up. The Go-constant comment
   in conditions.go (not CRD-fed) was updated.
D. Red run recorded against working-tree HEAD 8fea956 (task-9/10 commits
   atop 879f902); the three files the adjudication touches are byte-identical
   to 879f902's versions, verified by `git log --oneline -- <files>`.

## Commit ordering (for the wave meta)

No coupling outside my Target: api constant removal is self-contained
(no CRD schema change, no regeneration needed — verified the reason
string appears in no yaml). Docs amendment (deviation A) should land with
task-11; nothing here blocks or is blocked by task-9/10 files.
