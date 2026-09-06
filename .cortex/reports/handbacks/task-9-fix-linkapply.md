A2A: task-9 (fix-linkapply) complete — F7 reconciled, F10 landed, gates green. Changes uncommitted, scoped to internal/datapath. Full evidence: .cortex/plans/2026-09-06-kuport-implementation/task-9-gate-evidence.md (transcripts: .tmp/f7-red-run.txt, .tmp/f7-green-run.txt, .tmp/f7-kernel-run.txt, .tmp/task9-gates.txt).

## Files changed

- internal/datapath/netlink.go (M, +164): underlay extraction, linkActionFor decision, applyLinks retarget/rebuild branches, realNetlink.LinkSetUnderlay (minimal RTM_NEWLINK)
- internal/datapath/apply.go (M, +28): F10 reorder (links before nft), Apply doc, underlaySetter capability interface
- internal/datapath/apply_test.go (M, +20): fakeNL.LinkSetUnderlay
- internal/datapath/netlink_test.go (new): F7 behaviour tests, decision table, setter-absence convergence, address-shape normalisation pins
- internal/datapath/netlink_kernel_test.go (new): real-kernel netns proof, skip-guarded re-exec under unshare -rn
- git diff --stat (scoped to internal/datapath, plus the two new files untracked): 3 files, 206 insertions, 6 deletions

## Test-first: red then green

Red (F7 behaviour tests against unmodified netlink.go, `go test -run TestApplyLinks -count=1`):

```
--- FAIL: TestApplyLinksUpdatesStaleUnderlay (0.00s)
    netlink_test.go:69: existing link kept stale underlay: SrcAddr = 10.0.0.1, want 10.0.0.2
--- FAIL: TestApplyLinksUpdatesEachUnderlayParamAlone (0.00s)
    --- FAIL: .../remote_differs: underlay not reconciled to desired: group=10.0.0.8 vni=4242 port=4790
    --- FAIL: .../vni_differs:    underlay not reconciled to desired: group=10.0.0.9 vni=7 port=4790
    --- FAIL: .../port_differs:   underlay not reconciled to desired: group=10.0.0.9 vni=4242 port=1
```

Every failure names the missing comparison; the no-churn companion passed pre-fix, as it must.

Green (package, after fix): all 17 tests PASS, ok 0.652s, including TestApplyLinksUpdatesUnderlayOnRealKernel (0.68s), TestLinkActionFor, TestApplyIdempotent, TestGolden (goldens unchanged).

## Real-kernel proof (acceptance 2)

TestApplyLinksUpdatesUnderlayOnRealKernel re-execs under unshare -rn and runs applyLinks against 6.18.33.2-microsoft-standard-WSL2: changed LocalAddr on the existing kup-* link ends up desired after one apply, same device index, link stays UP, /31 intact, nothing deleted; fresh kernel dump re-reads as linkKeep (no update loop). Remote behaves the same. VNI and dst-port changes take the rebuild branch: name kept, index moves, same apply restores the /31. Skips where netns cannot open (CI unaffected).

## F10

Landed: Apply runs applyLinks before applyNFT (14 lines, local to apply.go; rules/routes stay last because they resolve device indexes). A mid-link failure leaves the previous nft ruleset untouched — refuse rather than forward to a pod whose replies cannot return. No ripple into render/reconcile; Apply signature unchanged; reconcile/agent callers (task-8's WIP included) compile and pass unchanged.

## Deviations (all evidence-backed)

1. netlink.LinkModify cannot implement the update: vishvananda v1.3.1 always sends IFLA_VXLAN_PROXY/RSC/L2MISS/L3MISS, and the kernel's changelink rejects those attributes by presence (vxlan_nl2flag), so whole-object modify is guaranteed EOPNOTSUPP (probe: all four attrs, live and down devices; .tmp/f7-probe-out.txt). RTM_SETLINK with link-info is a silent no-op (observed). Product uses realNetlink.LinkSetUnderlay: smallest RTM_NEWLINK carrying IFLA_IFNAME + IFLA_VXLAN_LOCAL/GROUP only; accepted live (iproute2-style control: .tmp/f7-ippattern-probe.txt).
2. Decision is three-way (keep/retarget/rebuild) not two-way: VNI and dst port cannot change on an existing device at all (kernel policy + probe), so rebuild is the only convergence path for identity changes; endpoint changes retarget in place per acceptance 2.
3. Seam: LinkSetUnderlay is an unexported optional interface (underlaySetter) asserted in applyLinks, not a NetlinkConn method — widening NetlinkConn broke internal/agent's committed fakeNetlink (a task-8/agent-owned file I must not edit; vet error at hostcheck_test.go:46 before the design fix). Non-implementers fall through to rebuild and still converge (TestApplyLinksNoSetterStillConverges).
4. Whole-repo go vet/golangci-lint currently fail only in internal/reconcile test files (reconcile_test.go:190 "declared and not used: c") — task-8's uncommitted WIP in this shared checkout, not task-9's: none of my files implicated, go build ./... passes over the same tree. My scoped gates: BUILD-OK, VET-OK (./internal/datapath/...), test ok, golangci-lint 0 issues.

Spec premise note for the record: "the existing apply tests" do not contain a committed netns harness (the review's netns runs were scratch); the suite now carries one, skip-guarded.

Going idle after this hand-back.
