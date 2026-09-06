# task-10 handback — fix-deploy-ci -> meta-wave4

All four changes landed, uncommitted. Gates green; one literal acceptance
command is unrunnable on this host by its own nature and has an equivalent
proof, plus one deliberate hardening of the F8 command form. Full diff of my
files at .tmp/task10/my.diff.

## Files changed (mine only; internal/datapath is task-8's)

| File | Change |
| --- | --- |
| chart/templates/_helpers.tpl | kuport.image fallback: `printf \"v%s\" .Chart.AppVersion` |
| chart/values.yaml | image.tag comment documents the v<appVersion> default |
| chart/README.md | checkout-install tag paragraph, Image reference order + OCI pull sentence, values table image.tag row |
| deploy/rbac.yaml | verbs narrowed to list/watch (cache kinds), update (status kinds); services rule removed |
| chart/templates/rbac.yaml | same |
| .github/workflows/ci.yaml | new envtest job (placed after generated, before image) |
| .github/workflows/release.yaml | OCI push step after Package the Helm chart; header comment names the push |

## git diff --stat (mine; siblings' internal/datapath excluded)

```
 .github/workflows/ci.yaml       | 30 +++++++++++++++++++++++
 .github/workflows/release.yaml  | 21 ++++++++++++++--
 chart/README.md                 | 15 ++++++++----
 chart/templates/_helpers.tpl    |  6 +++--
 chart/templates/rbac.yaml       | 25 +++++++++----------
 chart/values.yaml               |  3 ++-
 deploy/rbac.yaml                | 25 +++++++++----------
```

## Red runs (recorded before the fixes, .tmp/task10/render-before.yaml)

- F5: `image: ghcr.io/walzen-group/kuport-agent:0.1.0` (tag the pipeline never pushes)
- F6: get on all six watch kinds + `get`/`patch` on both status rules in both files; `resources: [\"services\"]` at deploy/rbac.yaml:27 and chart/templates/rbac.yaml:27
- F8: `grep -c envtest .github/workflows/ci.yaml` = 0
- OCI: `grep -c 'helm push' .github/workflows/release.yaml` = 0

## Green gates

1. F5: `nix develop -c helm template kuport chart/` ->
   `image: ghcr.io/walzen-group/kuport-agent:v0.1.0`.
   Scratch copy (.tmp/task10/chart-123, appVersion 1.2.3, empty image.tag) ->
   `:v1.2.3`. Explicit `--set image.tag=my-tag` -> `:my-tag`;
   `--set image.digest=sha256:deadbeef` -> `@sha256:deadbeef` (precedence intact).
2. F6: `helm lint chart/` -> `1 chart(s) linted, 0 chart(s) failed`
   (only the pre-existing icon INFO). No services match in either RBAC file;
   the only remaining get is the resourceNames-scoped configmaps rule
   (line 41 of each). kubectl gate: see deviation 1.
3. F8: `KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test -tags
   envtest -count=1 ./internal/agent/...` ->
   `assets=/home/nixos/.local/share/kubebuilder-envtest/k8s/1.37.0-linux-amd64`,
   `ok  github.com/walzen-group/kuport/internal/agent  13.622s`.
   Run against pristine HEAD export; see deviation 3.
   actionlint 1.7.12 (nix shell nixpkgs#actionlint; not in the flake) with
   shellcheck 0.11.0 wired in: both workflows clean.
4. OCI: `helm package chart/ --version 0.0.0-test --app-version 0.0.0-test
   --destination .tmp/` -> `Successfully packaged chart and saved it to:
   .tmp/kuport-0.0.0-test.tgz` (removed after; scratch only). Push lines
   verbatim (release.yaml:192-193):
   `helm registry login ghcr.io -u \"$GHCR_USER\" -p \"$GHCR_TOKEN\"` /
   `helm push \"kuport-$VERSION.tgz\" oci://ghcr.io/walzen-group/kuport`.
   A real ghcr push is not runnable here (no push from this host); the gate
   is syntax + exact path, named as the gap.

## RBAC drop evidence (production code grep)

- Only direct-API read anywhere: cmd/kuport-agent/main.go:138
  `agent.NewHost(mgr.GetAPIReader(), ...)` -> internal/agent/hostcheck.go:44
  Get of kube-system/cilium-config. The configmaps get rule covers exactly
  that and stays.
- Every other read is through the informer-cached client:
  internal/agent/inputs.go:27 List(nodes, namespaces, portmapclasses,
  portmaps, endpointslices); internal/agent/status.go:48,93,153 Get(portmap,
  class). Cached reads issue list+watch to the API server and never a get, so
  `get` is dropped on all five kinds.
- Status writes are internal/agent/status.go:66 and :187, both
  `Status().Update`; grep for `.Patch(` / `Status().Patch` / `Status().Get`
  matches production code nowhere. Status rules keep `update` only.
- services: no Get/List/Patch/Create of corev1.Service in any non-test file;
  the only consumer is the watch at internal/agent/agent.go:119, which
  task-8 removes.

## Deviations

1. Acceptance 2's literal `kubectl apply --dry-run=client -f deploy/rbac.yaml
   -f chart/templates/` cannot pass on this host and cannot pass as written,
   independent of my edits: kubectl v1.37 performs server discovery even for
   a client dry-run (documented in ci.yaml's chart job comment; the
   host kubeconfig's 127.0.0.1:62896 server is dead -> i/o timeout, exit 1,
   reproduced with --validate=false too), and kubectl cannot parse
   chart/templates/*.yaml while they are unrendered Go templates
   (`error parsing chart/templates/rbac.yaml: invalid character '{'`).
   Equivalent proof supplied: kubectl-validate (the repo's own offline
   validator from the pinned flake.lock nixpkgs) -> `deploy/rbac.yaml...OK`,
   full `helm template` render `...OK`.
2. The F8 step splits the spec's one-liner. `VAR=$(cmd) go test` is a prefix
   assignment: bash runs go test even when the substitution fails (verified
   on this host: under `set -euo pipefail`, `X=$(exit 3) true` continues),
   and the suite self-skips on an empty KUBEBUILDER_ASSETS -> green job with
   zero tests, the exact thing the spec forbids. Written as
   `assets=$(setup-envtest use 1.37.0 -p path)` then
   `KUBEBUILDER_ASSETS=\"$assets\" go test ...`; a standalone failing
   assignment exits the shell (verified: exit 3). Same command, fails loudly.
3. Gate 3 ran against a `git archive HEAD` export at .tmp/task10/head-tree
   because the working tree currently carries task-8's in-progress
   internal/datapath change which does not compile at check time
   (netlink.go: `Vxlan has no field Remote`, `LinkModify undefined`). My
   changes touch no Go; the committed tree's suite is green as quoted.
4. OCI real push not attempted (gap named per the spec).

## Commit ordering (mandatory)

task-8's services-watch removal (internal/agent/agent.go:119) is NOT
committed yet: HEAD e5ef0c8 still has the watch, and task-8's Go changes are
still landing. My RBAC files drop the services rule in the working tree.
So: commit the two RBAC files only in the same commit as, or later than, the
commit carrying task-8's watch removal - never alone before it. The rest of
my diff (verb narrowing except services, chart, CI) is safe to commit in any
order.
