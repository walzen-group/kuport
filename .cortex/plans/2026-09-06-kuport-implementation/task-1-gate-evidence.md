# Task 1 gate evidence — repo foundation

Recorded 2026-09-06 by impl-task1-foundation (Claude Code, herdr agent
impl-task1-foundation). Meta-wave1 read the run from that session's transcript
and cross-checked the artifacts. The gates were run once, by the worker; the
meta did not re-run them.

## Worker's recorded gate output

Run from /home/nixos/repos/kuport through `nix develop -c`:

```
go version go1.26.7 linux/amd64
controller-gen Version: v0.22.0
golangci-lint has version 2.13.2
helm v4.2.4
go build ./...   exit=0
go vet ./...     exit=0
golangci-lint run -> 0 issues.  exit=0
make generate    exit=0
make manifests   exit=0
make verify (diff -ru config/crd .tmp/crd) exit=0
grep -c "className is immutable" config/crd/*portmaps*.yaml -> 1
```

## Negative check, CEL immutability

No reachable cluster (docker-desktop context down), and kubectl 1.37 runs
discovery even for a client dry-run, so the literal dry-run apply cannot print
its expected output offline. The worker stood up a local control plane with
envtest (k8s 1.33.0 assets from setup-envtest) and proved server-side that both
CRDs install, a valid PortMap is accepted, and mutating spec.port is rejected
with: `PortMap.kuport.dev "gameserver" is invalid: spec: Invalid value:
"object": port is immutable`. Harness under .tmp/envtest-verify, gitignored and
build-tagged, not committed. The spec's grep check also passes (count 1).

## Deviations declared by the worker

1. groupversion_info.go uses the apimachinery-only SchemeBuilder
   (runtime.NewSchemeBuilder plus a central addKnownTypes) instead of the
   controller-runtime scheme.Builder, which controller-runtime v0.25 deprecates
   and golangci-lint/staticcheck SA1019 failed the gate on. AddToScheme and
   GroupVersion keep the same shape, so nothing downstream changes.
2. With controller-runtime out of the api package, nothing imports
   controller-runtime or k8s.io/api yet, and go mod tidy would drop them. A
   build-tagged tools.go pins all three dependencies so the project-wide
   baseline survives for tasks 2 to 6. The tag excludes it from build, vet,
   and lint.

Spec item 3's intent (all three dependencies pinned at coherent versions)
holds: sigs.k8s.io/controller-runtime v0.25.0 with k8s.io/api and
k8s.io/apimachinery v0.37.0, taken from controller-runtime v0.25's go.mod.

## Meta cross-checks

Done by meta-wave1 on 2026-09-06 after the worker settled:

- Staged file list matches the spec Target exactly: 15 files, nothing under
  internal/datapath, internal/reconcile, internal/agent, cmd, deploy, chart,
  .github, no Dockerfile. README.md, docs/, and .cortex/ stay untracked and
  untouched.
- flake.nix pins nixos-unstable and provides go_1_26, gopls, golangci-lint,
  kubernetes-controller-tools, kubectl, kustomize, kubernetes-helm,
  setup-envtest, git. The shell evaluates and each tool reports the version
  above.
- Type files match the spec sketches field for field, including every
  kubebuilder marker, the five CEL rules on PortMapSpec, and the list map keys
  on both statuses.
- Generated CRDs carry scope Cluster/Namespaced, shortNames pmc/pm, the
  defaults 1, 65535, 4242, 4790, 169.254.77.0/24, the CEL messages, and
  x-kubernetes-list-type map on links, nodes, and conditions.
- conditions.go exports every condition type and reason in the plan's table.
- Makefile targets generate, manifests, and verify run their tools through
  nix develop -c; verify regenerates into .tmp/crd and diffs against
  config/crd.

## Whole-change check, wave-1 scope

Run by meta-wave1; commands from the plan index's whole-change section that
task 1's scope reaches:

```
nix develop -c go build ./...                          exit=0
nix develop -c go vet ./...                            exit=0
nix develop -c controller-gen crd paths=./internal/api/... output:crd:dir=.tmp/crd-check
diff -r config/crd .tmp/crd-check                      empty
```

go test ./..., golangci-lint, helm lint, and the kubectl dry-run apply belong
to the whole-change green set of later waves; their absence here is expected,
not a failure.

## Commit

Commit 78b40f5 on branch feat/initial-implementation, created by meta-wave1:
the 15 files, 1745 insertions. master stays empty; history is append-only.
