# Task 2 gate evidence — datapath

Recorded 2026-09-06 by impl-task2-datapath (Claude Code, herdr agent
impl-task2-datapath), reported to meta-wave2 by A2A steer on completion and
cross-checked against that session's transcript. The gates were run once, by
the worker; the meta did not re-run them.

## Worker's recorded gate output

Run from /home/nixos/repos/kuport through `nix develop -c`:

```
go build ./...                          exit 0
go vet ./...                            exit 0
go test ./internal/datapath/... -v      all PASS; ok  github.com/walzen-group/kuport/internal/datapath
    Tests: TestApplyIdempotent, TestTeardownRemovesOwned, TestApplyReconcilesOwnedOnly,
    TestRenderShape (5 subs), TestSameNodeHasNoReturnPath, TestNFTExprsNonEmpty,
    TestDeterminism, TestGolden (5 subs), TestNFTText (5 subs), TestTableHeaders
go test ./internal/datapath/... -count=10   ok (determinism + idempotence stable)
golangci-lint run ./internal/datapath/...   0 issues
Golden nft syntax gate (all 5): nft -c -f <file> -> OK for every file
```

## Negative checks

- Idempotence: `TestApplyIdempotent` applies the same Plan twice against a
  fake handle and asserts the second pass issues no change.
- Determinism: `TestDeterminism` renders the same State twice and asserts
  byte-identical output; also clean at `-count=10`.
- `TestNFTExprsNonEmpty` asserts every renderable Rule yields a non-empty
  expression slice, driven from the same case table as the goldens, so a
  golden that passes while the applied expressions are wrong cannot happen.

## Deviations

1. `nft -c` needs root on this host: nftables 1.1.6 initializes a netfilter
   netlink cache even in check mode, so as uid 1000 the gate fails with
   "cache initialization failed: Operation not permitted". The worker ran it
   as `sudo -n nft -c -f` (passwordless sudo available) and all five goldens
   are accepted. The goldens are correct; only the invocation needs
   privilege. CI or the gate script should account for it.
2. `LinkAddr` renders as /31 per the pinned state.go contract (spec.md prose
   says /30). state.go is a netip.Prefix carrying its own length, so no code
   choice was baked in; the pinned contract was not changed.
3. go.mod gained github.com/google/nftables v0.3.0 and
   github.com/vishvananda/netlink v1.3.1 (looked up current); `go mod tidy`
   promoted golang.org/x/sys to direct. Transitives: mdlayher/netlink,
   mdlayher/socket, vishvananda/netns, google/go-cmp.
4. Product code never shells out to nft/ip — pure netlink + nftables
   libraries. `nft -c` appears only in the test gate as a syntax checker.

Committed by meta-wave2 as d7bfbd0 after verification, scoped to
internal/datapath/ + go.mod + go.sum.
