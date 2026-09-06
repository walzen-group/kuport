# Task 5 — Dockerfile, CI workflow, release workflow

Repo root: `/home/nixos/repos/kuport`. All paths relative to it.

## Context

kuport is a Kubernetes operator that runs as a DaemonSet, one agent per node,
writing nftables and netlink state on the host. Read `docs/spec.md`, sections
**Implementation notes** and **Build and release**.

You own how it gets built, tested and published. The consuming cluster pins by
tag and digest together — the digest is what decides, the tag is for people — so
the release has to report a digest and render manifests that carry it.

Tasks 1 and 4 have landed the Go module, the flake, the CRDs and the deploy
manifests. Task 6 is writing `cmd/kuport-agent` in parallel; **it may not exist
when you start**. Write the build against that path anyway and note in your
report which gates you could not run because the binary was absent.

## Target

Create:

- `Dockerfile`
- `.dockerignore`
- `.github/workflows/ci.yaml`
- `.github/workflows/release.yaml`
- `.golangci.yaml`

Do not touch: `internal/`, `cmd/`, `config/`, `deploy/`, `chart/`, `docs/`,
`README.md`, `flake.nix`, `go.mod`, `Makefile` (task 1 owns it; call its targets).

## Change

### 1. Dockerfile

Two stages. Build with the Go toolchain, ship a **static binary on `scratch`**
with no package manager and nothing fetched at start.

That last point is not stylistic. The predecessor this replaces ran an Alpine
image that installed iproute2 and nftables from the Alpine mirror on every pod
start, so a node rebooting while the mirror was unreachable came up with no rules
and the forward stayed down until the install worked. kuport speaks netlink and
nftables directly and needs nothing on the filesystem.

```
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kuport-agent ./cmd/kuport-agent
```

Final stage: `FROM scratch`, copy the binary, `USER 0` (it needs `NET_ADMIN`),
`ENTRYPOINT ["/kuport-agent"]`. No CA bundle is needed — the agent talks to the
in-cluster API server using the ServiceAccount token and the cluster CA, both
mounted by the kubelet. If task 6 turns out to need outbound TLS to anything
else, say so in your report rather than adding a CA bundle speculatively.

Look up the current Go image tag rather than writing one from memory. Pin it by
digest as well as tag.

`.dockerignore` excludes at minimum `.git`, `.tmp`, `.cortex`, `.direnv`,
`result*`, `docs`, `chart`, `deploy`.

### 2. `.golangci.yaml`

Enable at least `govet`, `errcheck`, `staticcheck`, `ineffassign`, `unused`,
`gofmt`, `goimports`, `misspell`, `bodyclose`, `errorlint`. Check the current
config schema version — golangci-lint v2 changed the file format, and a v1
config silently does less than you think on a v2 binary. Verify with
`nix develop -c golangci-lint config verify` if the installed version supports
it, and say which version the flake provides.

### 3. `ci.yaml`

Triggers: `push` to any branch, and `pull_request`.

Jobs, all on `ubuntu-latest`:

| Job | Runs |
|---|---|
| `check` | `go vet ./...`, `go test ./... -race`, `golangci-lint run`, `go build ./...` |
| `generated` | `make verify` — regenerates CRDs and fails on drift |
| `image` | builds the image for `linux/amd64`, does **not** push |
| `chart` | `helm lint chart/`, `helm template chart/`, and a client-side dry-run apply of the render |

Use the Nix flake rather than per-tool setup actions, so CI and a developer's
shell run identical versions:

```yaml
- uses: actions/checkout@v5
- uses: cachix/install-nix-action@v31
  with:
    extra_nix_config: |
      experimental-features = nix-command flakes
- uses: DeterminateSystems/magic-nix-cache-action@v13
- run: nix develop -c go test ./... -race
```

Check the current major of every action before pinning it; the versions above
are illustrative and several of these move. Pin each action by tag.

The `generated` job is the one that stops generated files drifting from source,
so it must fail the build rather than warn. Make that explicit.

### 4. `release.yaml`

Trigger: `push` on tags matching `v*`.

Permissions: `contents: write`, `packages: write`, `id-token: write`.

Steps, in order:

1. Derive `VERSION` from `github.ref_name` with the leading `v` stripped.
   Reject a tag that is not semver, with a clear failure message.
2. Run the full `check` gate. A tag that does not pass CI does not become a
   release.
3. Build and push `ghcr.io/${{ github.repository_owner }}/kuport-agent` for
   `linux/amd64`, tagged `:v<version>` and `:<major>.<minor>`. Capture the
   pushed **digest** from the build step's output.
4. Write the digest into the job summary (`$GITHUB_STEP_SUMMARY`), because that
   is where a person looks for it.
5. Render `deploy/` with the image pinned **by digest**:
   `kustomize edit set image ghcr.io/<owner>/kuport-agent=ghcr.io/<owner>/kuport-agent@<digest>`
   then `kubectl kustomize deploy/ > kuport-<version>.yaml`.
6. Package the chart with `version` and `appVersion` set to the release version
   and `image.digest` defaulted to the pushed digest, producing
   `kuport-<version>.tgz`.
7. Create the GitHub Release for the tag and attach `kuport-<version>.yaml`, the
   chart tarball, and the CRDs as a separate `crds-<version>.yaml`. Put the
   image reference **and its digest** in the release body.

Versioning is semver on the agent. The CRD version moves separately and only for
a schema change, which is what `v1alpha1` in the group means — do not tie the
two together.

Do not add signing, SBOM generation, or provenance attestation. They are
reasonable things to want and none of them were asked for; note them in your
report as candidates if you like, but do not build them.

### 5. Concurrency and caching

Add a `concurrency` group per workflow keyed on the ref, cancelling in-progress
runs for CI but **not** for release. A cancelled release mid-push leaves a tag
with a partial image.

## Constraints

1. **Toolchain is entered, never assumed.** No `go`, `helm`, `kubectl` or
   `kustomize` on the host PATH. Local commands run as `nix develop -c <cmd>`.
2. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push. The wave's meta-agent
   makes one commit per task once that task's gate passes, so parallel delegates
   never race one git index. **Create no git tag**, at any point: a tag fires the
   release workflow against a repo with no remote.
3. **Stay inside your Target.**
4. **Look up current versions before pinning.** Every GitHub Action, the Go
   base image, the golangci-lint config schema. Writing a version from memory
   pins the wrong schema and wastes a round trip.
5. **The image is a static binary on scratch.** No package manager, nothing
   fetched at container start.
6. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`**, never `/tmp`, never
   `.cortex/`.
7. **Human-facing prose follows `catalyst-v2-writing-docs`**, humanizer pass
   included, for workflow comments and any prose you write.
8. **Acceptance criteria are inviolable.** If one cannot be met, stop and report
   it with the criterion intact.
9. **Report as a diff, not a commit**: files changed, `git diff --stat`, verbatim
   gate output, deviations.
10. **Emission discipline**: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.

## Acceptance

There is no GitHub remote and no Actions runner, so the workflows cannot execute
here. Verify what can be verified locally, and be explicit in your report about
what remains unproven.

```
nix develop -c golangci-lint run
nix develop -c bash -c 'command -v actionlint && actionlint .github/workflows/*.yaml || echo "actionlint unavailable"'
nix shell nixpkgs#yamllint -c yamllint -d relaxed .github/workflows/
nix shell nixpkgs#actionlint -c actionlint .github/workflows/ci.yaml .github/workflows/release.yaml
docker build -t kuport-agent:local .
docker run --rm --entrypoint /kuport-agent kuport-agent:local --version
```

The last two need `cmd/kuport-agent` to exist (task 6). If it does not yet,
say so and run them once it does, or report them as blocked with the exact
command you would run.

**Negative checks**, both required:

- `docker run --rm --entrypoint /bin/sh kuport-agent:local -c true` must
  **fail**. A scratch image has no shell, and a passing run means a base image
  crept in.
- `docker image inspect kuport-agent:local --format '{{.Size}}'` should be under
  50MB. A number in the hundreds of megabytes means the build stage leaked into
  the final image.

Green is: `actionlint` clean on both workflows, `yamllint` clean,
`golangci-lint` clean, the image building, `--version` printing the injected
version, and both negative checks behaving as described.

## Report to

Your dispatch brief names the wave's meta-agent. Send your completion hand-back
to it with `c2d steer --agent <meta> "A2A: ..."`, not to the orchestrator.
