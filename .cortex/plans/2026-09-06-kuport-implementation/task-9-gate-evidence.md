# task-9 gate evidence — F7 link underlay comparison, F10 apply ordering

Worker: fix-linkapply, wave 4. Tree: feat/initial-implementation tip e5ef0c8,
changes left uncommitted. Environment: kernel 6.18.33.2-microsoft-standard-WSL2.

## Files changed

```
 internal/datapath/apply.go              |  28 +++-   (Apply reorder + underlaySetter seam note)
 internal/datapath/netlink.go            | 163 +++++-  (underlay, linkActionFor, applyLinks branches, LinkSetUnderlay)
 internal/datapath/apply_test.go         |  20 +      (fakeNL.LinkSetUnderlay)
 internal/datapath/netlink_test.go       | new        (F7 behaviour, decision table, setter-absence and address-shape pins)
 internal/datapath/netlink_kernel_test.go| new        (real-kernel netns proof, skip-guarded)
```

testdata/ goldens untouched; TestGolden passes unchanged.

## Red run (F7, recorded before the fix)

`go test ./internal/datapath/ -run TestApplyLinks -count=1` against
unmodified netlink.go:

```
--- FAIL: TestApplyLinksUpdatesStaleUnderlay (0.00s)
    netlink_test.go:69: existing link kept stale underlay: SrcAddr = 10.0.0.1, want 10.0.0.2
--- FAIL: TestApplyLinksUpdatesEachUnderlayParamAlone (0.00s)
    --- FAIL: TestApplyLinksUpdatesEachUnderlayParamAlone/remote_differs (0.00s)
        netlink_test.go:100: underlay not reconciled to desired: group=10.0.0.8 vni=4242 port=4790
    --- FAIL: TestApplyLinksUpdatesEachUnderlayParamAlone/vni_differs (0.00s)
        netlink_test.go:100: underlay not reconciled to desired: group=10.0.0.9 vni=7 port=4790
    --- FAIL: TestApplyLinksUpdatesEachUnderlayParamAlone/port_differs (0.00s)
        netlink_test.go:100: underlay not reconciled to desired: group=10.0.0.9 vni=4242 port=1
FAIL
FAIL	github.com/walzen-group/kuport/internal/datapath	0.004s
```

The fails-for-the-right-reason check: every case fails because the comparison
does not exist at HEAD; the no-churn companion test
(TestApplyLinksLeavesMatchingUnderlayAlone) already passed pre-fix, as it must.
Full transcript: .tmp/f7-red-run.txt.

## Green run (same package, after the fix)

```
--- PASS: TestApplyIdempotent (0.00s)
--- PASS: TestTeardownRemovesOwned (0.00s)
--- PASS: TestApplyReconcilesOwnedOnly (0.00s)
--- PASS: TestApplyLinksUpdatesUnderlayOnRealKernel (0.68s)
--- PASS: TestApplyLinksUpdatesStaleUnderlay (0.00s)
--- PASS: TestApplyLinksUpdatesEachUnderlayParamAlone (0.00s)
--- PASS: TestApplyLinksLeavesMatchingUnderlayAlone (0.00s)
--- PASS: TestLinkActionFor (0.00s)
--- PASS: TestUnderlayOf (0.00s)
--- PASS: TestApplyLinksNoSetterStillConverges (0.00s)
--- PASS: TestRenderShape (0.00s)
--- PASS: TestSameNodeHasNoReturnPath (0.00s)
--- PASS: TestNFTExprsNonEmpty (0.00s)
--- PASS: TestDeterminism (0.00s)
--- PASS: TestGolden (0.00s)
--- PASS: TestNFTText (0.00s)
--- PASS: TestTableHeaders (0.00s)
PASS
ok  	github.com/walzen-group/kuport/internal/datapath	0.686s
```

## Real-kernel proof (F7 acceptance 2)

TestApplyLinksUpdatesUnderlayOnRealKernel re-executes the test binary under
`unshare -rn` and runs applyLinks on realNetlink against the kernel:

- create link with local 10.0.0.1;
- change desired LocalAddr to 10.0.0.2, one apply: kernel holds 10.0.0.2, same
  device index, link still UP, /31 still attached, exactly one kup-* device;
- the kernel's fresh dump re-reads as linkKeep (no update loop on next apply);
- endpoint changes (local, remote) take the in-place path on the live device;
- identity changes (VNI 7777, port 12345) take the rebuild path: name kept,
  index moves, same apply restores the /31, and the fresh dump re-reads as
  linkKeep.

Verbatim: .tmp/f7-kernel-run.txt (PASS, inner run in netns).
On hosts that cannot open a netns the test skips; the CI suite is unaffected.

## Why not netlink.LinkModify (spec deviation, empirically grounded)

The spec suggested LinkModify on the Vxlan object. Probes (.tmp/f7probe/,
.tmp/f7-probe-out.txt, .tmp/f7-ippattern-probe.txt) plus the kernel source
(vxlan_nl2flag, vxlan_nl2conf in drivers/net/vxlan/vxlan_core.c):

| change | LinkModify (library v1.3.1) | minimal RTM_NEWLINK, live device |
|---|---|---|
| local address | EOPNOTSUPP | works |
| remote (group) | EOPNOTSUPP | works |
| VNI | EOPNOTSUPP | rejected ("Cannot change VNI"), also down |
| dst port | EOPNOTSUPP | rejected ("Cannot change port"), also down |

vishvananda v1.3.1 addVxlanAttrs always sends IFLA_VXLAN_PROXY/RSC/L2MISS/
L3MISS; the kernel's changelink rejects those attributes by presence
(vxlan_nl2flag, changelink_supported=false), so whole-object modify can never
succeed on any VXLAN. RTM_SETLINK with link-info is a silent no-op. The product
path is therefore realNetlink.LinkSetUnderlay: the smallest RTM_NEWLINK that
carries IFLA_IFNAME plus IFLA_VXLAN_LOCAL/GROUP only. This is a bounded,
documented deviation from the spec's suggested API; the behaviour it must
produce is exactly what the spec asked for.

The decision is three-way (keep / retarget / rebuild) instead of the spec's
two-way (no-op / update) because the kernel cannot change VNI or destination
port on an existing device at all; rebuild is the only convergence path for
those, and the /31 and routes are restated by the same apply.

## Seam: underlaySetter is optional (constraint-4 decision)

Adding LinkSetUnderlay to datapath.NetlinkConn broke internal/agent's
committed fakeNetlink (internal/agent/hostcheck_test.go:46) — a file this task
must not edit. The capability is therefore an unexported optional interface
that applyLinks type-asserts: realNetlink and the datapath fakes implement it;
anything else falls through to the rebuild path and still converges
(TestApplyLinksNoSetterStillConverges pins that). The pinned State seam is
untouched.

## F10

Apply now runs applyLinks before applyNFT (rules and routes stay last, as they
resolve device indexes). A mid-link failure leaves the previous nft ruleset
untouched, so the node refuses instead of DNAT-ing toward a pod whose return
link is not live; a failure after the commit only ever concerns rules over an
already-live link. The change is 14 lines in apply.go, no ripple into render or
reconcile (Apply signature unchanged; callers and goldens pass unchanged: see
green run — TestApplyIdempotent, TestTeardownRemovesOwned, TestGolden).

## Gates (acceptance 4)

```
nix develop -c go build ./...                              OK
nix develop -c go vet ./internal/datapath/...              OK (silent)
nix develop -c go test ./internal/datapath/... -count=1    ok 0.667s
nix develop -c golangci-lint run ./internal/datapath/...   0 issues.
```

Whole-repo `go vet ./...` and `golangci-lint run` currently fail only inside
internal/reconcile test files (reconcile_test.go:190 "declared and not used:
c"). Those lines belong to task-8's uncommitted work-in-progress in the shared
checkout (git status shows reconcile/, agent/, chart/, workflows files modified
by that worker), not to task-9: none of my changed files is implicated, and
`go build ./...` passes over the same tree. Running the project-wide suites was
also excluded by the spec's gate note.

## Scratch (experiments that close the named gaps)

- .tmp/f7-red-run.txt — the red transcript
- .tmp/f7-green-run.txt — package green
- .tmp/f7-kernel-run.txt — netns inner run
- .tmp/f7probe/main.go, .tmp/f7-probe-out.txt — per-attribute modify matrix on
  a live device (library path)
- .tmp/f7-ippattern-probe.txt — per-attribute matrix via iproute2-style
  minimal messages, incl. the down-device control
- .tmp/vxlan_core.c — kernel source read used to explain the rejections
