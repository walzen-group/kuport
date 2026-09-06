# Task 3 gate evidence — reconcile

Recorded 2026-09-06 by impl-task3-reconcile (Claude Code, herdr agent
impl-task3-reconcile), reported to meta-wave2 by A2A steer on completion and
again on the meta's resend request (the first steer's text did not reach the
meta session). Cross-checked against the session transcript. The gates were
run once, by the worker; the meta did not re-run them.

## Worker's recorded gate output

Run from /home/nixos/repos/kuport through `nix develop -c`:

```
go build ./...      silent, ok
go vet ./...        silent, ok
go test -v -count=1 ok 0.009s — 13 tests / 37 subtests all PASS
go test -count=10   ok 0.037s
golangci-lint       0 issues
```

## Files

11 files, 2397 insertions: reconcile.go, conflicts.go, endpoints.go,
links.go, status.go + six test files. All under internal/reconcile/; staged,
uncommitted.

## Negative checks

- TestDeterminismUnderShuffle: seeded RNG shuffles Inputs slices, runs
  Compute 20 times, asserts every Result deeply equal to the first.
- TestAgentAgreement: one fixture with three nodes, Compute per node, the
  three agree on chosen endpoint, accepting node, and link slot.

## Deviations (interpretations of under-specified edges)

1. statusOwner: PortMapStatus writer = endpoint holder if an endpoint is
   chosen, else first accepting node by name, else first cluster node by
   name. Extends the single-writer rule to endpoint-less/refused mappings so
   NoReadyEndpoint/ClassNotFound still get exactly one deterministic writer.
2. buildClassStatus: a non-accepting endpoint holder still emits ClaimLinks
   (spec requires the holder, usually a target node, to propose the claim);
   its ClassContribution.Node is left zero (no Nodes row). Only accepting
   nodes emit Nodes row, GC, and class conditions.
3. Subnet exhausted -> class Ready=False/SubnetExhausted only (class-level
   per spec); the affected mapping simply gets no link device.
4. TunnelOK/CNIVxlanPort left off Inputs; tunnel-mode and vxlan-port
   collision class conditions left to task 6, as the spec permits.
5. PublishedAddress.Address left empty for task 6 to fill from the host
   (spec: iface->address not resolvable from the API alone).

None reduce the task's observable purpose.

Committed by meta-wave2 after verification, scoped to internal/reconcile/.
