# Final whole-branch review — kuport, feat/initial-implementation

Reviewer: review-whole-branch (fresh eyes, no code authored). Tree reviewed at
tip 7eda6be (nine commits — a rename commit landed mid-review; see F4). All
gates were re-run at the tip and pass. Where a claim rests on a host
experiment, the experiment is named.

## Findings

### F1 — Endpoint choice is per-agent where the contract needs cross-agent agreement. BLOCKING for multi-replica Services.
- internal/reconcile/endpoints.go:34-41 (candidate on this node wins), consumed
  by statusOwner internal/reconcile/status.go:131-134.
- If the referenced Service has ready endpoints on two or more nodes, each
  agent's local preference selects its own node's pod. The architecture premise
  (spec.md:260-262 "every agent computes from the same inputs ... they agree
  without talking") and the index's "deterministic rather than negotiated"
  contract both break. Consequences: the accepting node DNATs to pod X while
  the remote node programs marks/links for pod Y that never receives DNAT'd
  traffic; two nodes believe they own the PortMap status and fight every pass;
  needed-pair sets (and therefore link claims and GC) differ across agents, so
  class status churns too.
- docs/spec.md:292-294 states the rule ("A candidate on this node wins...
  which is stable across agents") and is itself the contradiction. The spec is
  the datapath authority; this is a spec-level defect the implementation
  inherited, so it is for the design owner, not the implementer.
- Trigger is the default shape of the product's own example: `gs-0` in the
  smoke fixture names a Deployment; any 2-replica Service hits it.
- Tests do not cover it: TestAgentAgreement (reconcile_test.go:160) runs three
  viewpoints of a single-endpoint fixture; endpoints_test.go runs one node.
- Confidence: high (complete code trace; not empirically run — needs a
  multi-node world fixture).

### F2 — The status contract for readiness is not implemented: NodeStatus.Ready is dead and Programmed=True never checks reports.
- internal/reconcile/status.go:33 hardcodes `Ready: true`; `Message` is never
  set by anything; no code path checks interface presence on the host
  (agent interfaceAddrs/status.go:126-140 drops unresolvable interfaces
  silently, row stays ready).
- Spec requires otherwise: class status example docs/spec.md:225-228
  (`ready: false`, `message: interface wt0 not present`); PortMap Programmed
  "True when every accepting node has written its rules" (spec.md:195) with
  reason NodeNotReady for a node that has not (spec.md:196); operations.md:80
  documents exactly that check. The only NodeNotReady use is
  endpoints.go:125-130, for "class selects zero nodes" — a different meaning.
- decideRole sets Programmed=True from role resolution alone (endpoints.go:136
  and :159). No reader of class status.nodes readiness exists anywhere; the
  condition is therefore true while an accepting node is down or has no agent.
- Related lie in the same field: during claim propagation the target node's
  slot is unknown (reconcile.go:242-247), so it programs the mark rule but no
  divert or link, and still reports Programmed=True — replies leave via the
  node's own uplink with the pod address as source, the exact failure the spec
  calls "quietly broken" (spec.md:492-495). It self-heals in seconds, but
  status is false during the window it matters.
- Confidence: high (the first three bullets are absence-of-code, checked by
  grep across reconcile+agent).

### F3 — The multi-accepting-node refusal contradicts operations.md and disables the internal-class pattern.
- endpoints.go:147-153: with len(accepting) >= 2, every agent gets
  Programmed=False RemotePodMultipleAcceptingNodes and only accepting[0]
  programs — including the case where the pod sits on a non-first accepting
  node, and the case where accepting[0] itself is fully programmed locally.
- docs/operations.md:81 documents the refusal as applying when "the pod is on
  none of them" and says the first node programs (implying it reports
  True); docs/spec.md:629-631 justifies the refusal with "not needed while an
  internal class has a pod on every accepting node" — that sanctioned pattern
  does not work as described: nodes 2..n never DNAT, so the port answers only
  on the first node's addresses. integration.md:214-221's intranet-class
  example (mode None dodges it) shows the author knew, but the mode Vxlan
  multi-accept internal case is left broken-but-green.
- Confidence: medium-high (code trace firm; which reading of the refusal is
  intended is a spec question).

### F4 — API group rename landed mid-review; verified complete.
- Brief described eight commits; tip gained 7eda6be "rename API group
  kuport.dev to kuport.wlz.li" (2026-09-06 12:12). Before that commit,
  README/integration examples said kuport.wlz.li while code registered
  kuport.dev — a docs-vs-code break that made every example manifest invalid.
  The rename fixes it. Re-checked: groupversion_info, all six CRD copies,
  both RBAC files, samples, kustomizations, docs — no stale kuport.dev
  remains in tracked files (only gitignored .tmp scratch). Full gates at the
  new tip: build, vet, go test -count=1, make verify (CRDs match generated),
  helm lint, tagged envtest run against the 1.37 control plane — all green.
- Confidence: high (executed).

### F5 — Chart's default image tag is never published.
- chart/templates/_helpers.tpl:58-63 resolves the default image to
  `repository:appVersion` — e.g. ghcr.io/walzen-group/kuport-agent:0.1.0.
  .github/workflows/release.yaml:108-110 pushes `v<version>` and `<minor>`
  only. `helm install ./chart` straight from the repo (chart/README.md:24)
  references a nonexistent tag. The release tarball masks it because
  release.yaml patches image.digest in (163-177), and deploy/ uses the `v`
  form (daemonset.yaml:38). One-line fix: `printf "v%s"` the appVersion
  fallback, or push both tag forms.
- Confidence: high on the mismatch; medium on blast radius (source installs
  only).

### F6 — RBAC is broader than the code's calls.
- deploy/rbac.yaml:11-34 and chart/templates/rbac.yaml:11-34: `get` is granted
  on portmapclasses, portmaps, endpointslices, services, nodes, namespaces and
  on both status subresources; every read goes through the informer cache
  (inputs.go list, fillPublished/writePortMap Get are cache-served), so list
  and watch are the only verbs needed for those. `patch` on the status
  subresources is unused — writes are Status().Update only.
- The `services` resource itself exists only because SetupWithManager watches
  corev1.Service (agent.go:119), and no Service is ever read: Inputs has no
  Services field, gatherCandidates matches the slice label. The spec's own
  watch list (spec.md:256) has five kinds; the code's sixth watch adds a
  cluster-wide informer and its RBAC for no consumer.
- The configmaps rule is the good citizen: get-only, resourceNames-scoped,
  uncached reader — matches the single host check.
- Severity: hygiene, but the brief makes any over-broad grant a finding.
  Confidence: high for verbs; medium that the Service watch is unintended
  rather than defensive (I found no justification in any doc).

### F7 — Node address change is not reconciled on existing links.
- netlink.go:50-57: applyLinks keys only on link name; a link that exists is
  never compared against Link.LocalAddr/RemoteAddr/VNI/Port. A node whose
  InternalIP changes (the normal condition on home connections with dynamic
  public addresses — this product's stated deployment) keeps the stale
  underlay on every existing kup-* device; traffic stops while status stays
  green, and only deleting the device on the node heals it. Teardown/restart
  also fixes it (start removes and rebuilds? no — crash keeps, clean restart
  tears down and next apply rebuilds ✓). So an agent restart repairs it; a
  silent per-node address change does not get repaired by reconcile.
- Confidence: high on the code behavior (comparison is absent); medium on
  real-world frequency.

### F8 — CI does not run the envtest tier, contrary to docs/spec.md:788.
- ci.yaml runs `go test ./...` with no tag; envtest_test.go is
  `//go:build envtest` (self-skips). "GitHub Actions runs the unit, golden and
  envtest tiers" is not true as wired; spec.md:7's "CI is wired to run them"
  overstates the same. A doc sentence the code does not honour.
- Confidence: high (read of both workflow files).

### F9 — mark 0x6b700000 doubles as "slot 0" and "slot unknown".
- reconcile.go:239-247 slotOrZero returns 0 when the slot is unresolved;
  links.go:177 builds a real slot-0 mark from the same value. A node holding a
  slot-0 return link and a second mapping whose claim has not landed marks
  both flows 0x6b700000; the second mapping's replies divert onto the slot-0
  link toward the wrong accepting node. Needs slot 0 claimed plus an
  unresolved second mapping on the same node — narrow, self-healing. Fix:
  resolve-unknown could emit the mark rule with a reserved sentinel, or omit
  the mark until the divert exists.
- Confidence: medium (logic solid, window short).

### F10 — Partial applies across the nft/netlink boundary self-heal but are unguarded.
- apply.go:66-80 commits nft first, then links, rules, routes; a mid-netlink
  failure returns an error (nft already rewritten) and Reconcile requeues.
  Level-driven design covers it; no rollback is needed, but F2's green
  conditions mean the half-programmed window is invisible. Ordering link
  before nft for remote mappings would shrink the window (accepting node would
  refuse rather than forward to a pod whose replies can't return).
- Confidence: medium.

## Checked and sound

- State contract: internal/datapath/state.go is field-for-field the pinned
  contract in the plan index; reconcile's emitters and datapath's renderers
  agree on every field's semantics, including ExemptRule.PortIsSrc (accepting
  dport, target sport) and Negate (cilium_* wildcard on the target,
  cilium_host exact on the accepting node).
- Golden rulesets byte-match the spec's smoke test and the hand-built
  rulesets, including the `kup-mangle` empty on accepting nodes, the
  port-range single-match rendering, and the `0x6b700001` mark.
- Naming/numbering table honoured end to end: kup-<sha256[0:8]>, 200+slot,
  0x6b700000|slot, prefs 101/102, /31 by sorted pair, first-sorted node takes
  the lower end. Verified against code and against the emitted values.
- The loop guard is real: loopGuardRule emits pref 101 with the peer node
  /32 → main; netns experiment (real datapath.Apply under `unshare -rn`):
  both rules present in `ip rule list`, and `ip route get 8.8.8.8 mark
  0x6b700001` resolves via table 201 → default via 169.254.77.2 dev
  kup-peer0000. Unmarked lookup correctly falls through to main.
- FRA_FWMASK worry disproven empirically: netlink v1.3.1 RuleAdd sends no
  mask attr when Rule.Mask is nil; the kernel stores 0xffffffff and the rule
  matches. (The probe dump showed mask=0xffffffff on the added rules.)
- VXLAN zero-value Learning=false (library always sends the attr, verified in
  source at link_linux.go:1233 and in the live link dump) does not break the
  point-to-point link: two-netns probe with nolearning + unicast remote
  crossed 3/3 pings with an empty FDB (kernel floods unknown unicast to the
  configured remote). TX/RX error counters flat.
- Fresh-link first-pass (LinkByName fails → LinkAdd → AddrList/AddrAdd on the
  zero-index object) works on the real kernel; ensureIndex resolves by name.
  APPLY OK, address present, link up.
- Determinism: no time.Now, no math/rand, no goroutines outside tests
  (grep); every output slice sorted by a total key at both the reconcile
  (sortState, key builders) and render (sortRules, links/rules/routes sorts)
  layers; shuffle-agreement test passes; map iterations are all re-sorted or
  order-insensitive.
- Status concurrency: writes are read-fresh-merge-retry loops on the whole
  status subresource; resourceVersion is the server-side arbiter, so a stale
  write conflicts rather than clobbers; mergeClaim refuses to overwrite
  another pair's slot and defers; dropStaleClaim re-checks staleness on the
  fresh object. The retry re-reads the informer cache, not the API — worst
  case a conflict retry exhausts and the level-driven requeue fixes it; no
  lost update. Tested explicitly (status_test "concurrent agent's row not
  clobbered"). Envtest run confirms against a real apiserver.
- Conflict precedence (creationTimestamp, then namespace/name bytewise) and
  slot claims (lowest free, sorted key order, holder-only for unlanded) are
  the same function on every agent and match the index.
- CRD parity: config/crd, deploy/crds, chart/crds byte-identical to each
  other and to freshly generated output (make verify clean at tip).
- Probes: /healthz + /readyz on 8081 in both deploy trees match main.go
  defaults; readyz gated on first completed reconcile — sound.
- Metrics server disabled (BindAddress "0"), no extra RBAC consumed; no lease
  rules; leader election off — matches architecture claims.
- Teardown runnable blocks on ctx and tears down with a live context; crash
  leftovers reconciled by name/mask ownership — Teardown removes only kup-*
  links and 0x6b70**-marked rules; foreign objects untouched (verified in
  apply_test TestApplyReconcilesOwnedOnly and code).
- release.yaml digest-pin grep contract matches deploy/kustomization image
  name; chart packaging patches the digest for the tarball; plain-semver tag
  gate rejects suffixes as documented.

## Blocking statement

F1 is blocking: it breaks the no-central-coordination contract for any Service
with ready endpoints on multiple nodes, including the spec's own Deployment
example, and produces fighting status writers. F2/F3 are blocking-adjacent —
the conditions table in the plan index is a pinned contract and the code
currently cannot produce its documented NodeNotReady / ready=false semantics.
The datapath itself, the seams that route packets, checked sound.
