# Development

Building, testing and releasing the agent, plus the implementation choices a
reader of the code will want explained.

## Repo layout

```
cmd/kuport-agent/          the binary: flags, manager wiring, signals
internal/api/v1alpha1/     CRD types, deepcopy, validation
internal/reconcile/        the pure desired-state computation
internal/agent/            informers, host checks, status writes, teardown
internal/datapath/         nftables and netlink, the only package that touches the host
internal/datapath/testdata golden rulesets
config/crd/                generated CRD manifests
config/samples/            a PortMapClass and a PortMap
deploy/                    DaemonSet, ServiceAccount, ClusterRole, published per release
chart/                     the Helm chart, same contents parameterized
docs/                      see docs/README.md
.github/workflows/         ci.yaml, release.yaml
```

`internal/datapath` is the only package allowed to touch the host. Everything
above it takes a desired-state struct and returns one, which is what makes the
reconcile testable without a cluster or root.

## Implementation notes

### Speak netlink and nftables directly

Use `github.com/google/nftables` and `github.com/vishvananda/netlink` rather than
shelling out to `nft` and `ip`. The image is then the binary on a scratch base,
with no package manager and nothing fetched at startup.

That last point matters. The predecessor this replaces ran an alpine image that
installed iproute2 and nftables from the Alpine mirror on every pod start, so a
node rebooting while the mirror was unreachable came up with no rules and the
forward stayed down until the install worked.

### nftables reserves words

`mark` and `fwd` cannot be used as chain names; the parser rejects them. Both
were found the hard way, one of them by a ruleset that had never run before.
Prefix kuport's chain names to stay clear of the grammar entirely.

### Idempotence and ownership

kuport owns exactly the objects it names:

| Object | Named |
| --- | --- |
| nftables table | `kuport`, family ip |
| nftables chains | `kup-pre`, `kup-post`, `kup-mangle` |
| vxlan links | `kup-` + first 8 hex of sha256(class name and peer node name), 12 characters |
| routing tables | `200 + slot`, the slot recorded in the class status |
| routing rules | pref 101 loop guard, pref 102 divert, marks 0x6b700000 or'd with the slot |
| masquerade and mark rules | inside kuport's own chains, which are rewritten whole each pass |

Every pass rewrites the whole table in one transaction. Links, rules and routes
are reconciled by comparison, since they have no transaction. Anything the agent
finds under its own names and does not want is removed.

A link device is corrected in place where the kernel allows it. Its endpoint
addresses and its MTU can be changed on a running device; its VNI and destination
port cannot, and those rebuild it. A rebuild drops the /31 and every route
pointing at the old index, which the same pass restores.

On shutdown the agent removes its table, links, rules and routes, so a node taken
out of a class stops holding state nobody wants. A crashed agent leaves them, and
the next start reconciles them away.

### What the agent refuses

| Condition | Behaviour |
| --- | --- |
| the link's UDP port collides with the CNI's | report `Ready=False` with `VxlanPortConflict` on the class |

The CNI's routing mode was a second refusal until v0.4.0, when both legs moved
onto kuport's own link and the mode stopped deciding anything.

A class that selects several nodes refuses nothing on that ground alone. How many
of them forward a given mapping is [servingMode](datapath.md#serving-modes).

## Testing

**Unit.** The desired-state computation, given a set of PortMapClasses,
PortMaps, EndpointSlices, Nodes, Namespaces and a node name. Table-driven, no
cluster, no root. This is where endpoint choice, conflict ordering, class
selection, link slot allocation, MTU sizing and every refusal are covered.

**Golden rulesets.** The datapath package renders a desired state to the exact
nftables ruleset and netlink objects it would apply, compared against files in
testdata. A change to a rule is then visible in a diff during review, which
matters for rules whose failure mode is a silent drop. Regenerate with
`go test ./internal/datapath -run TestGolden -update`.

**envtest.** The controller against a real API server with no nodes, covering
status writing, conditions and the watch plumbing. The CEL immutability rules
were proved server-side this way.

**End to end.** A kind cluster cannot exercise this: the whole design is about
what happens between two machines joined by a mesh. The end-to-end tier runs by
hand against a real cluster, with an echo responder behind a PortMap probed from
outside, asserting the reply names the prober's own address. The cluster repo
carries those fixtures and the probe script.

Run everything a push would run, before tagging:

```sh
nix develop -c go build ./...
nix develop -c go test ./...
nix develop -c golangci-lint run
make verify
```

`golangci-lint` is the one that catches what `go test` does not. A v0.3.0 release
went out with an unused function because only the tests were run locally.

## Build and release

GitHub Actions.

**ci.yaml**, on push and pull request. The check job runs `go vet`,
`go test ./... -race`, `golangci-lint run`, and `go build ./...` under
`nix develop -c`, so CI and a developer shell use the same toolchain. A second
job runs `make verify`, which regenerates the CRDs and fails on any diff against
committed `config/crd/`, so generated files cannot drift. A third job runs the
envtest tier against a real API server, with the kubebuilder assets pinned to
1.37.0. A fourth builds the image for linux/amd64 without pushing, and a fifth
lints and renders the Helm chart and dry-run applies the render against the API
schemas.

`chart/crds/` is a hand-kept copy of `config/crd/`. `make manifests` writes only
the latter, so a schema change needs the copy updated as well.

**release.yaml**, on a tag matching `v*` (plain semver; suffixes are rejected):
build and push the image to `ghcr.io/walzen-group/kuport-agent`, tagged with the
version and the rolling minor, with the digest reported in the job summary. Then
three artifacts are attached to the GitHub Release: `kuport-<version>.yaml`, the
rendered `deploy/` tree with the image pinned to that digest;
`kuport-<version>.tgz`, the packaged chart; and `crds-<version>.yaml`, the CRD
bundle, because the schemas move on their own schedule. The packaged chart is
also pushed to `oci://ghcr.io/walzen-group/kuport` as an OCI artifact at the
release version, with the image digest baked into its values, so a GitOps
consumer can pull the chart by reference. The chart's own image default is the
tag `v<appVersion>`, which is the form a release pushes; installing the chart
from a source checkout needs a published release or an explicit tag or digest.

Consumers pin by tag and digest together. The digest is what decides; the tag is
for people.

Versioning is semver on the agent. The CRD version moves separately and only for
a schema change, which is what `v1alpha1` in the group means.

## Deployment

The cluster's own infrastructure repo consumes a release: the CRDs and the
DaemonSet as manifests, the image pinned by digest, and the PortMapClass objects
written from that cluster's inputs. kuport ships the manifests and the image and
knows nothing about any particular cluster.
[integration.md](integration.md) is the guide for bringing it into one.
