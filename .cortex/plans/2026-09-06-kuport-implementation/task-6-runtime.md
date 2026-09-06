# Task 6 — Agent runtime: informers, status writing, link claims, main

Repo root: `/home/nixos/repos/kuport`. All paths relative to it.

## Context

kuport delivers a TCP or UDP port from one node's addresses to a pod on another
node, keeping the client's source address. Read `docs/spec.md`, especially
**Architecture**, **The reconcile**, **Status writing**, and **What the agent
must refuse**.

One DaemonSet, one agent per node, **no central controller and no leader
election**. Every agent watches the same objects and computes independently with
deterministic tie-breaks, so they agree without talking to each other.

Three pieces already exist and you wire them together:

- `internal/api/v1alpha1` (task 1) — the CRD types, condition constants.
- `internal/datapath` (task 2) — `State`, `Render`, `Apply`, `Teardown`,
  `LinkMTU`. The only package that touches the host.
- `internal/reconcile` (task 3) — `Compute(Inputs) Result`, pure.

You own the shell around them: informers in, `Compute`, `Apply` out, status
back. Plus the host checks that `Compute` cannot make because they need root.

There is no `go` on the host PATH; use `nix develop -c`. **There is no test
cluster in this session** — envtest is authored and run if its assets fetch, and
that is the ceiling. Do not claim anything ran against a cluster.

## Target

Create:

- `cmd/kuport-agent/main.go`
- `internal/agent/agent.go` — the manager, watches, the reconcile trigger
- `internal/agent/inputs.go` — cache reads into `reconcile.Inputs`
- `internal/agent/status.go` — status writes with conflict retry
- `internal/agent/hostcheck.go` — tunnel mode, VXLAN port collision, MTU
- `internal/agent/*_test.go`
- `internal/agent/envtest_test.go` — behind a build tag or an env guard

Do not touch: `internal/api/`, `internal/datapath/`, `internal/reconcile/`,
`config/`, `deploy/`, `chart/`, `.github/`, `Dockerfile`, `docs/`, `README.md`,
`flake.nix`. If one of them needs a change, stop and report it.

## Change

### 1. `main.go`

Flags, each with an env fallback so the DaemonSet can set either:

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--node-name` | `NODE_NAME` | — | required; the DaemonSet sets it from `spec.nodeName` via the downward API |
| `--log-level` | `LOG_LEVEL` | `info` | |
| `--resync` | `RESYNC` | `5m` | informer resync, the level-driven safety net |
| `--health-addr` | `HEALTH_ADDR` | `:8081` | |
| `--version` | — | — | print the version injected at build time and exit 0 |

`--version` must work: task 5's image gate runs
`docker run --rm --entrypoint /kuport-agent kuport-agent:local --version`.
Declare `var version = "dev"` in package main; the build injects it with
`-X main.version=`.

Serve `/healthz` and `/readyz` on `--health-addr`. Ready means the informer
caches have synced and at least one reconcile has completed. Tell task 5 and
task 4 the port in your report so the DaemonSet probe matches; task 4 was told
to leave the probe out rather than guess.

### 2. Watches and the reconcile trigger

Use `sigs.k8s.io/controller-runtime`. Watch `PortMapClass`, `PortMap`,
`EndpointSlice`, `Service`, `Node`, `Namespace`.

The reconcile is **level-driven and whole-node**, not per-object. Every watch
event maps to the same fixed request key, so any change recomputes the complete
desired state for this node. Do not write a per-PortMap reconciler: a mapping
that was deleted must disappear by being absent from the next `State`, and a
per-object loop reintroduces exactly the remembering-to-remove-it that the
design avoids.

Coalesce bursts. An endpoint moving changes the input on every agent at once, so
debounce events into a single recompute — controller-runtime's rate limiter on a
single key does this for you; do not hand-roll a timer.

Scope the caches. `Node` can be filtered to this node **only** for host checks;
class selection needs every node's labels and InternalIP, so that informer stays
cluster-wide. Do not filter it and then wonder why link endpoints are empty.

### 3. Host checks — `hostcheck.go`

These need the host and so cannot live in `internal/reconcile`. Each maps to a
`PortMapClass` condition:

| Check | How | Condition on failure |
|---|---|---|
| Cilium in tunnel mode | read the `cilium-config` ConfigMap in `kube-system`: `routing-mode` must be `tunnel` (older keys: `tunnel` set to `vxlan` or `geneve`). Absent ConfigMap means Cilium is not the CNI — report it, do not assume. | `TunnelModeRequired` |
| return-link port collides with the CNI's | compare `returnPath.vxlan.port` with the CNI's tunnel port, 8472 for Cilium | `VxlanPortConflict` |
| MTU | read the InternalIP-bearing interface's MTU through netlink; `LinkMTU` is `datapath.LinkMTU(underlay)` | none; reported in `status.nodes[]` |

Native routing instead of tunnelling breaks the inbound leg outright, because
the encapsulation is what carries a client address past WireGuard's source
check. Refuse every mapping in that class rather than failing mysteriously.

Reading the ConfigMap needs `get` on `configmaps` in `kube-system`. That is not
in task 4's RBAC — report it so the ClusterRole gets a narrow rule for it, and
say exactly which rule you need. Do not edit `deploy/` yourself.

MTU deserves the operator's attention: on the cluster this was designed against
the mesh carries 1350, Cilium is at 1350, a pod crosses to another node at 1300,
and the link's 50 bytes bring a 1300-byte reply to exactly 1350. Zero margin.
Report the numbers; do not try to fix it.

### 4. Applying

Call `reconcile.Compute`, then `datapath.Apply` with the returned `State`.

On `SIGTERM` and `SIGINT`, run `datapath.Teardown` before exiting, so a node
taken out of a class stops holding state nobody wants. A crashed agent leaves
its objects and the next `Apply` reconciles them away — that is by design, so do
not add a startup-only cleanup path that fights it.

Log every applied change at info, and the no-op passes at debug. A datapath
whose failure mode is a silent drop needs a log line that says what it wrote.

### 5. Status writing

**PortMap status.** Write only the entries `Compute` returned in
`Result.PortMapStatus` — the agent holding the chosen endpoint, and no other.
That gives exactly one writer per PortMap without leader election, and it changes
hands when the endpoint moves.

Fill in `Published[].Address` where `Compute` left it empty: that is the address
of the named interface on the accepting node, which only that node's agent can
read. An accepting node that holds no endpoint therefore has an address the
status writer needs and cannot see. Resolve this by having each accepting agent
publish its own interface addresses into `PortMapClass.status.nodes[]` (extend
the row with an `addresses` map of interface name to address, and **report the
API change** — do not edit `internal/api` yourself), and have the status writer
read them from there. If you find a simpler resolution, take it and say what you
did.

**PortMapClass status.** Each agent maintains its own `nodes[]` row. The
endpoint-holding agent additionally writes `links[]` claims. Both go through the
same path.

**Conflict retry is the whole mechanism, so get it right.** Use
`retry.RetryOnConflict` with `DefaultRetry`. On conflict, **re-read and
recompute the patch from the fresh object** — never replay a stale one. Use the
status subresource. Prefer a merge patch on the specific list entry over a full
status replace, so two agents updating different rows do not clobber each other.

**Link claims.** `Result.ClassStatus[class].ClaimLinks` are proposals. Before
writing one, re-read the class and check the slot is still free; if another agent
took it, recompute rather than overwriting. `DropLinks` removes entries whose
`unusedSince` is more than 24h old. Any agent may run the drop; a lost race is
harmless because the next pass re-evaluates.

### 6. Tests

Unit tests with a fake client (`sigs.k8s.io/controller-runtime/pkg/client/fake`)
for: inputs assembly from a populated cache, status writing including a forced
conflict-and-retry, link claim collision between two agents, and each host check
against a fabricated ConfigMap.

The conflict test is the important one. Drive a real 409 through the fake client
and assert the second attempt recomputed from the fresh object. A retry that
replays a stale patch passes a naive test and corrupts status in production.

`envtest` covers status writing, conditions and the watch plumbing against a
real API server with no nodes. Guard it so `go test ./...` passes when the assets
are absent:

```
nix develop -c setup-envtest use -p path
```

If that fetches, wire `KUBEBUILDER_ASSETS` and run it. If it does not, skip with
`t.Skip` on the missing binary and **say so plainly in your report** — an envtest
that silently skips and reports green is the failure this instruction prevents.

There is no cluster. Do not write, and do not run, anything that needs one.

## Constraints

1. **Toolchain is entered, never assumed.** Every Go command runs as
   `nix develop -c <cmd>` from the repo root.
2. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag. The wave's
   meta-agent makes one commit per task once that task's gate passes, so
   parallel delegates never race one git index.
3. **Stay inside your Target.** You will find two things needing changes outside
   it (the ClusterRole's `configmaps` rule, and the `NodeStatus` addresses
   field). **Report both; do not make them.**
4. **Never shell out to `nft` or `ip`.** `internal/datapath` speaks netlink
   directly and is the only package that touches the host. The image is a static
   binary on scratch with no package manager.
5. **No leader election, no Lease objects.** If you find yourself reaching for
   one, the design has been misread — re-read **Status writing** in the spec.
6. **`go test ./...` must pass with no cluster and no root.**
7. **Look up current versions before pinning** any dependency you add.
8. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`**, never `/tmp`, never
   `.cortex/`.
9. **Human-facing prose follows `catalyst-v2-writing-docs`**, humanizer pass
   included, for package doc comments that run to prose.
10. **Acceptance criteria are inviolable.** If one cannot be met, stop and
    report it with the criterion intact. In particular: do not claim a cluster
    test ran.
11. **Report as a diff, not a commit**: files changed, `git diff --stat`,
    verbatim gate output, deviations.
12. **Emission discipline**: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.

## Acceptance

From `/home/nixos/repos/kuport`. Paste verbatim output into your report.

```
nix develop -c go build ./...
nix develop -c go vet ./...
nix develop -c go test ./... -race -count=1
nix develop -c golangci-lint run
nix develop -c go run ./cmd/kuport-agent --version
nix develop -c bash -c 'setup-envtest use -p path || echo "envtest assets unavailable"'
```

Green is: build and vet silent, every test `ok`, `golangci-lint` clean,
`--version` printing a version string and exiting 0.

**Negative checks**, all three required:

- **No leader election crept in.**
  `grep -rn 'LeaderElection\|coordination.k8s.io\|leases' cmd/ internal/agent/`
  returns nothing. controller-runtime enables leader election by default in some
  manager option sets; this catches it.
- **The conflict retry recomputes.** Your status test must fail if the retry is
  changed to replay the stale patch. Demonstrate this: break it deliberately,
  paste the failing output, restore it, paste the passing output. A retry test
  that passes both ways is not a test.
- **Teardown actually runs on SIGTERM.** A test that sends the signal to the
  runnable and asserts `Teardown` was called on the fake datapath handle. An
  agent that leaves its rules behind on a clean shutdown is a real bug and an
  easy one to ship.

## Report to

Your dispatch brief names the wave's meta-agent. Send your completion hand-back
to it with `c2d steer --agent <meta> "A2A: ..."`, not to the orchestrator.

State plainly in the report: whether envtest ran or skipped, and the two
out-of-scope changes you are handing back (the `configmaps` RBAC rule and the
`NodeStatus` addresses field).
