# task-10 — Chart image tag, RBAC verbs, OCI chart push, envtest in CI (F5, F6, F8, OCI)

Execution skill: `catalyst-v2-sdd-rules` where a behavior has a checkable
outcome; global constraints below reproduced from the plan index.

## Context

The final whole-branch review found three defects in the deploy/CI surface,
and the user approved one addition:

- F5: the chart's default image tag resolves to the appVersion with no `v`
  prefix (`ghcr.io/walzen-group/kuport-agent:0.1.0`), a tag the release
  pipeline never pushes (it pushes `v0.1.0` and `0.1`). `helm install ./chart`
  straight from the repo references a nonexistent tag.
- F6 (RBAC part): `deploy/rbac.yaml` and `chart/templates/rbac.yaml` grant
  `get` on kinds whose every read goes through the informer cache (only
  list/watch are needed) and `patch` on status subresources no code uses (all
  writes are `Status().Update`). task-8 removes the `corev1.Service` watch and
  the `services` rule; you own every other RBAC narrowing.
- F8: CI never runs the envtest tier. `envtest_test.go` is build-tagged and
  self-skips; spec.md's claim that GitHub Actions runs unit, golden and
  envtest tiers is false as wired.
- OCI (user-approved 2026-09-06): `release.yaml` gains a step that pushes the
  packaged chart to ghcr.io as an OCI artifact, so Flux-style consumers pull
  `oci://ghcr.io/walzen-group/kuport` by version with the digest pin intact.

## Target

Files you may edit:

- `chart/templates/_helpers.tpl` (image helper, F5)
- `chart/values.yaml` (image.tag default documentation, F5)
- `chart/README.md` (install/image text that F5 and OCI change; the rest of
  chart docs is task-11's)
- `deploy/rbac.yaml` and `chart/templates/rbac.yaml` (F6 in full: verb
  narrowing AND the `services` rule removal. task-8 removes the Go watch only;
  the RBAC files are yours alone in this wave)
- `.github/workflows/ci.yaml` (new envtest job, F8)
- `.github/workflows/release.yaml` (OCI chart push step)

Explicit non-goals: no `internal/` code (task-8 owns the watch removal and
anything in Go); no docs/spec.md, docs/operations.md, docs/integration.md or
root README.md edits (task-11 owns them).

## Change

Read first: the two RBAC files, `chart/templates/_helpers.tpl`,
`chart/values.yaml`, `chart/README.md`, `.github/workflows/ci.yaml`,
`.github/workflows/release.yaml`, and the review report's F5/F6/F8 passages.
The envtest suite lives in `internal/agent/envtest_test.go` (build tag
`envtest`); read its header for the exact run command.

### F5 — default image tag carries the v prefix

In `chart/templates/_helpers.tpl`, the `kuport.image` fallback becomes the
appVersion with a `v` prefix:

```
{{ .Values.image.repository }}:{{ .Values.image.tag | default (printf "v%s" .Chart.AppVersion) }}
```

`chart/values.yaml` documents the default as `v<appVersion>` (example:
`v0.1.0`) and that a release-published artifact overrides via `image.digest`.
`chart/README.md`'s install section states that a plain `helm install ./chart`
from a git checkout pulls the default tag, which exists only after the
matching release was cut, and that consumers pin `image.tag` or `image.digest`
otherwise.

### F6 — RBAC matches the code's actual calls

Inventory the client calls first (grep `client.Get`, `client.List`,
`client.Patch`, `Status().Update`, `Status().Patch`, watches) and narrow the
RBAC to exactly what the code issues:

- Informer-cache-backed reads: `list`, `watch` only. Drop `get` on kinds whose
  reads are all cache-served (portmaps, portmapclasses, endpointslices, nodes,
  namespaces).
- Status subresources: keep only the verb the code uses (expected `update`
  from `Status().Update`); drop `get` and `patch` when nothing calls them.
- The `configmaps` rule stays as the good citizen it is: get-only,
  resourceNames-scoped, uncached reader.
- Remove the `services` rule entirely: task-8 removes the `corev1.Service`
  watch in the same wave, so no service verb is needed after both land. If
  your gate passes before task-8's watch removal is committed, hold your RBAC
  hunk and tell the meta to commit task-8's watch removal first; the tree must
  never drop the services rule while the watch still exists.

Prove each dropped verb by grep evidence in your report. If a drop would break
a real call, keep the verb and say why.

### F8 — envtest tier in CI

Add a job to `.github/workflows/ci.yaml` that runs the tagged envtest suite
against a real API server on a runner:

```
nix develop -c bash -c 'KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test -tags envtest ./internal/agent/...'
```

`setup-envtest` is already in the flake dev shell; on a fresh runner it
downloads the 1.37.0 control-plane assets (network is available there). Match
the file's existing style: permissions block, cache action, one named step
that fails the job on error. The envtest suite skips itself when assets are
unavailable, so the job must be written to fail loudly if the asset fetch
fails (the command above fails when `use` cannot fetch).

### OCI — release.yaml pushes the chart to ghcr

After the existing "Package the Helm chart" step, add a step that logs helm
into ghcr with the actions token and pushes the packaged chart:

```
helm registry login ghcr.io -u ${{ github.actor }} -p ${{ secrets.GITHUB_TOKEN }}
helm push kuport-<version>.tgz oci://ghcr.io/walzen-group/kuport
```

The packaged tarball already carries the release version, appVersion and the
baked `image.digest` (the package step patches a throwaway chart copy). Match
the file's existing patterns (env for credentials, the same quoting style as
the neighboring steps, `set -euo pipefail` where the pattern uses it). The
artifact path is the `kuport-$VERSION.tgz` the package step produced at the
repo root. Keep the GitHub Release attachment as it is.

## Constraints (reproduced from the plan index; all apply)

1. Toolchain is entered, never assumed. Every command runs as `nix develop -c`
   from `/home/nixos/repos/kuport`, or inside `nix develop`.
2. Committing is the wave meta-agent's job. Leave changes uncommitted; `git
   add` is fine. No commit, no branch, no push, no tag.
3. Append-only git discipline: no `git reset`, `git rebase`,
   `git commit --amend`, or history reordering.
4. Stay inside your Target.
5. `internal/datapath` is the only package that touches the host; you touch no
   Go code.
6. Never shell out to `nft` or `ip`.
7. nftables reserves `mark` and `fwd`; chain names are `kup-`-prefixed. Not
   your concern unless a render you check regresses, in which case report.
8. Scratch goes to `/home/nixos/repos/kuport/.tmp/`, never `/tmp`, never
   `.cortex/`.
9. Human-facing prose follows `catalyst-v2-writing-docs` (load it before
   writing or editing any README or comment block that reads as prose;
   apply its humanizer pass to the prose you touch).
10. Acceptance criteria are inviolable. If one cannot be met, stop and report.
11. Report as a diff, not a commit: files changed, `git diff --stat`, verbatim
    gate output, deviations.
12. Emission discipline: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.
13. Report completion to the wave's meta-agent named in your dispatch brief via
    `c2d steer --agent <meta> "A2A: ..."`, never to the orchestrator.

## Acceptance

1. F5: `nix develop -c helm template kuport chart/` renders the agent image as
   `ghcr.io/walzen-group/kuport-agent:v0.1.0` with no overrides, and as
   `:v1.2.3` when `--set image.tag` is empty with appVersion 1.2.3 in a
   scratch copy (or equivalent assertion).
2. F6: `nix develop -c helm lint chart/` clean; `nix develop -c kubectl apply
   --dry-run=client -f deploy/rbac.yaml -f chart/templates/` succeeds; grep
   evidence in the report proves each dropped verb has no caller and no
   `services` rule remains in either RBAC file.
3. F8: the envtest suite passes locally with the assets present:
   `nix develop -c bash -c 'KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 -p path) go test -tags envtest ./internal/agent/...'`
   and the workflow change passes actionlint (`nix develop -c actionlint` if
   available in the flake, else report the version you checked with and how).
4. OCI: `release.yaml` is syntactically valid and actionlint-clean; the pushed
   chart artifact path and registry are exactly
   `oci://ghcr.io/walzen-group/kuport`. A real push is not runnable here; name
   that gap. The full chart job renders locally:
   `nix develop -c helm package chart/ --version 0.0.0-test --app-version 0.0.0-test --destination .tmp/` succeeds (scratch only, never committed).
5. Do not run project-wide test suites; the Go surface is task-8's.

## Report-to

The wave meta-agent named in your dispatch brief. Report: files changed,
`git diff --stat`, gate output verbatim, deviations. After sending the
completion steer, run no further commands and go idle.
