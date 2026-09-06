# Task 5 gate evidence — Dockerfile, CI, release

Recorded 2026-09-06 by impl-task5-ci (omp, herdr agent impl-task5-ci), reported
to meta-wave2 by A2A steer on completion and cross-checked against that
session's transcript. The gates were run once, by the worker; the meta did not
re-run them.

## Worker's recorded gate output

Run from /home/nixos/repos/kuport:

```
actionlint (nix shell nixpkgs#actionlint -c actionlint ci.yaml release.yaml):
    clean, exit 0 ("ACTIONLINT-OK"). The flake-shell form prints "actionlint
    unavailable" (not in devShell); the nix-shell form is the gate.
yamllint -d relaxed .github/workflows/:   exit 0, 7 line-length warnings only
    (>80 cols, relaxed = warning level).
golangci-lint config verify:              OK. Flake provides golangci-lint
    2.13.2 -> v2 schema (version: "2", gofmt/goimports under formatters).
golangci-lint run ./internal/api/...:     "0 issues."
golangci-lint run (full tree):            fails ONLY on sibling WIP untracked
    packages: internal/reconcile (undefined resolveMappings etc) and
    internal/datapath (text_test.go:67 unknown field Negate at run time).
    Not task 5's Target; gate goes green when tasks 2/3 land.
```

Docker gates — BLOCKED, cmd/kuport-agent still absent (task 6 owns it):

```
docker build -t kuport-agent:local .
docker run --rm --entrypoint /kuport-agent kuport-agent:local --version
docker run --rm --entrypoint /bin/sh kuport-agent:local -c true   (must fail)
docker image inspect kuport-agent:local --format "{{.Size}}"      (expect <50MB)
```

Dockerfile mechanics proven meanwhile via a .tmp stub-context probe against
the real Dockerfile: build OK, "kuport-agent probe-9.9.9" printed from the
VERSION ldflags, /bin/sh run failed exit 127 (no shell in scratch), probe size
1,585,314 bytes. Probe image and directory cleaned up.

## Negative checks

- The scratch-image no-shell check cannot run until the image builds; the
  stub probe's exit-127 on /bin/sh already demonstrates the shape, and the
  real run is queued for when task 6 lands the binary.
- Image size under 50MB likewise deferred to the real build.

## Looked-up pins (2026-09-06, all verified against the APIs)

actions/checkout v7.0.1, cachix/install-nix-action v31.11.1,
DeterminateSystems/magic-nix-cache-action v14 (spec example v13 stale),
docker/setup-buildx-action v4.3.0, docker/login-action v4.6.0,
docker/build-push-action v7.3.0, softprops/action-gh-release v3.0.3.
Go image: golang:1.26.8-alpine3.24@sha256:ce864e…a1628 (flake go is 1.26.7;
image stage deliberately one patch ahead, the current release).

## Deviations

1. kustomize v5.8.1 `edit set image` has no directory flag (verified
   locally: unknown flag), so release.yaml does `cd deploy && kustomize edit
   set image … && cd .. && kubectl kustomize deploy/ > kuport-$VERSION.yaml`,
   then greps the render for `@sha256:` and fails loudly if deploy/ image
   names do not match the pushed ref. The grep is the contract check against
   task 4's manifests.
2. The release check gate mirrors ci.yaml's check-job commands inline rather
   than reusing ci.yaml via workflow_call, because the spec fixes ci.yaml
   triggers at push + PR; a sync-maintenance comment sits in the file.
3. Chart packaging assumes chart name `kuport` and a values key
   `image.digest` (spec contract); it packages a patched .tmp copy, source
   chart/ untouched, and the output tgz is renamed to kuport-<version>.tgz
   regardless. yq via nixpkgs#yq-go (mikefarah v4.53.3).
4. `id-token: write` declared per spec but unused today; no signing/SBOM/
   attestation added — candidates only, not built.
5. No CA bundle in the scratch image per spec; flagged for task 6 review: if
   the agent ever needs outbound TLS beyond the in-cluster API server this
   needs revisiting.
6. Semver gate rejects prerelease/build suffixes with a clear ::error:: (the
   shared major.minor tag and chart version assume stable releases).
7. CI image job: build-push-action push:false, linux/amd64, gha layer cache.
   Chart job does helm lint, helm template, and a client-side dry-run apply
   of the render via nix develop.

Staged by the worker; not committed by it. Commit is the meta-agent's, in
task order.

## Wave 3 completion — four docker gates run (2026-09-06, by meta-wave3)

cmd/kuport-agent landed at a1cee38 (task 6). The queued gates ran from
/home/nixos/repos/kuport against that tree:

```
docker build -t kuport-agent:local .            -> exit 0 (build log .tmp/docker-build.log)
docker run --rm --entrypoint /kuport-agent kuport-agent:local --version
                                                 -> "dev", exit 0
docker run --rm --entrypoint /bin/sh kuport-agent:local -c true
                                                 -> exit 127: runc: exec: "/bin/sh":
                                                    no such file or directory (must fail: PASS)
docker image inspect kuport-agent:local --format "{{.Size}}"
                                                 -> 33353890 bytes (~31.8 MB) < 50 MB: PASS
```

All four gates pass. "dev" is the Dockerfile's unstamped VERSION default
(ARG VERSION=dev); CI stamps the real version via release.yaml.

Re-run by meta-wave3 on the final tree (4055a27, post follow-up commit):
same four gates, all pass; size 33357986 bytes (~31.8 MB). The image gate
evidence above therefore anchors to the shipped tree, not the pre-follow-up
one.
