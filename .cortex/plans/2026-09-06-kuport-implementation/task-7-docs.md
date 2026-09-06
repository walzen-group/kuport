# Task 7 — Spec update with diagrams, operations guide, integration guide

Repo root: `/home/nixos/repos/kuport`. All paths relative to it.

## Context

kuport delivers a TCP or UDP port from one node's addresses to a pod on another
node, keeping the client's source address, and lets the workload ask for it
itself. It is now built: API types, a pure reconcile, an nftables/netlink
datapath, a DaemonSet agent, CI, releases, deploy manifests and a Helm chart.

You are writing the documentation that ships with it. Three audiences, and they
want different things:

- **The spec** (`docs/spec.md`) is the design record. It exists and is good. Your
  job is to close its open questions with the decisions that were made, add the
  diagrams the routing needs, and make sure it describes what was actually built.
- **The operations guide** (`docs/operations.md`) is for a person with a mapping
  that is not working.
- **The integration guide** (`docs/integration.md`) is for a **future coding
  agent** installing kuport into a specific cluster. That agent starts blank and
  has no access to this conversation.

Read `docs/spec.md` first, then the code, before writing anything.

## Target

Edit:

- `docs/spec.md`
- `README.md`

Create:

- `docs/operations.md`
- `docs/integration.md`

Do not touch: `internal/`, `cmd/`, `config/`, `deploy/`, `chart/`, `.github/`,
`Dockerfile`, `flake.nix`, `go.mod`. If the code contradicts the spec, **the code
wins for describing what exists** — write down what is there and flag the
discrepancy in your report. Do not change code to match a document.

## Change

### 1. `docs/spec.md` — close the open questions

The **Open questions** section is closed. Replace it with a **Decisions** section
recording each answer and its reasoning. These are settled; do not reopen or
hedge them.

| Question | Decision | Reasoning to record |
|---|---|---|
| Port ranges | `spec.port` required, `spec.endPort` optional and inclusive, mirroring NetworkPolicy. One nftables `dport <first>-<last>` match. | A single port needs no new field, a range needs no hundred objects, and the shape is one people already know. |
| No ready endpoint | Remove the DNAT rule. The port refuses — TCP reset, ICMP port-unreachable — rather than blackholing. `Programmed=False`, reason `NoReadyEndpoint`. | A client learns immediately instead of hanging until timeout. Connections do not survive a rollout, which is the accepted cost. |
| Link address allocation | A recorded claim in `PortMapClass.status.links`, not a computed hash. `/31` per node pair. The endpoint-holding agent allocates and writes; the accepting agent reads. The API server's optimistic concurrency is the arbiter. | The API server is already a consistent store with compare-and-swap, so no leader, no election, and no agent-to-agent protocol is needed. Collisions become impossible and the allocation is visible in `kubectl get portmapclass -o yaml`. It reuses the single-writer rule the design already has for status. |
| Link release | Freed after 24h unused; any agent the class selects may run the GC. | The link *device* is torn down immediately by being absent from the next desired state. The entry lingers only to hold the address reservation, so nothing in the free window carries traffic and the GC needs no locking. Clock skew is irrelevant against 24h. |
| Firewall | kuport writes `nat` and `mangle` in its own table and never anything in `filter`, and does not detect foreign filter rules either. | Writing the rule and detecting a foreign one have the same portability problem across nftables, iptables-legacy, firewalld and ufw, and a wrong "looks fine" is worse than no check. A workload-authored PortMap must not punch a hole in the host firewall. |

Leave **Other CNIs** as a genuine open question; it was not decided.

Also bring the spec in line with what was built: the `endPort` field, the
`status.links` and `status.nodes` shapes on `PortMapClass`, the condition reasons
as implemented, and the `kup-` chain names. Check each against the code.

### 2. `docs/spec.md` — the diagrams

Add mermaid diagrams. They are the point of this task: the routing is the part
that does not survive prose alone. Use fenced ```mermaid blocks.

**Diagram 1 — the cross-node packet path.** The one that matters most. Client to
accepting node, DNAT to the pod address, Cilium's vxlan carrying it past
WireGuard's source check, the pod, then the reply's separate path back through
the return link. Label each hop with what changes: which address is rewritten,
which is preserved, which encapsulation is added. A reader should be able to see
why the reply does not simply retrace the request.

**Diagram 2 — the same-node path**, beside it for contrast. DNAT and the
masquerade exemption only, no link, no mark, no policy routing. This makes the
cross-node machinery legible by showing what is absent when it is not needed.

**Diagram 3 — the routing rule ladder.** A packet arriving at the target node's
egress and walking the `ip rule` prefs in order: kuport's loop guard at 101,
kuport's divert at 102, netbird's resolve at 105, netbird's catch-all at 110,
`main` at 254. Show the two paths that matter — the marked reply taking the
divert, and the encapsulated outer packet being caught by the loop guard and sent
to `main`. This diagram is the one that would have saved the debugging session
behind it, so make it earn its place: an outer packet that reaches 102 is routed
back into the device it just left, and the kernel refuses silently while bumping
`tx_errors`.

**Diagram 4 — the reconcile.** Watched objects into `Compute`, out to
`datapath.State`, applied as one nftables transaction plus reconciled netlink
objects, with the status write branching off. Show that every agent runs the same
computation and that only the endpoint holder writes status.

**Diagram 5 — link allocation.** A short sequence diagram: target agent reads the
class, finds no claim for the pair, writes the lowest free slot, loses a race and
retries, accepting agent reads the settled claim and brings up its half.

Keep each diagram small enough to read. Five focused diagrams beat two sprawling
ones. Every node label must correspond to something real in the code or a rule in
the spec — a diagram that describes an idealised version of the system is worse
than none.

### 3. `docs/operations.md`

For a person whose mapping is not working. Cover:

- **Reading status.** What `kubectl get portmap` and `-o wide` show, what
  `published` means, and that the absence of a row is itself information.
- **Every condition reason**, one row each, with what it means and the first
  thing to check. Take the list from `internal/api/v1alpha1/conditions.go` —
  every reason there gets a row.
- **The counters.** Each nftables rule carries `counter`; show how to read them
  (`nft list table ip kuport`) and what a zero counter at each stage tells you.
  A packet that never reaches the DNAT counter is a different problem from one
  that reaches it and never arrives.
- **The failure that leaves no log line.** The VXLAN routing loop increments the
  device's `tx_errors` and logs nothing anywhere. Say how to spot it
  (`ip -s link show kup-*`) and what causes it.
- **The host firewall.** kuport writes nothing in `filter`. A node with a host
  firewall needs the port opened there, by whatever manages that firewall, and
  kuport will report everything as healthy while the port stays shut. Say this
  plainly; it is the most likely first-install surprise.
- **MTU.** How to read the numbers from `status.nodes[]` and what a zero margin
  means. Cilium sets a pod's interface MTU to the underlay MTU rather than 50
  below it, so a pod is always told 50 more than it can send between nodes. TCP
  absorbs this through packetization-layer path MTU discovery in blackhole mode;
  UDP does not, so a workload sending large datagrams needs its own send size
  kept under the real figure. A game server is exactly the workload that hits
  this.
- **Agent lifecycle.** Shutdown removes the table, links, rules and routes. A
  crashed agent leaves them and the next start reconciles them away. What to
  expect during a rolling update.

### 4. `docs/integration.md` — for the next agent

This is the deliverable the user asked for by name: instructions for a future
agent integrating kuport into a cluster as an operator. Write it **to that
agent**, plainly, assuming it has the cluster's repo in front of it and nothing
else.

It must cover:

**Preconditions, checkable before anything is installed.** Each with the exact
command and the answer that means go:

- Cilium in tunnel mode. `kubectl -n kube-system get cm cilium-config -o
  jsonpath='{.data.routing-mode}'` must be `tunnel`. Native routing breaks the
  inbound leg outright, because the encapsulation is what carries a client
  address past WireGuard's source check. This is a hard stop, not a warning.
- Pod addresses routable between nodes.
- The mesh accepting node-to-node traffic on the chosen VXLAN port, which must
  differ from the CNI's 8472.
- MTU headroom. Show the arithmetic: mesh MTU, CNI MTU, real pod-to-pod figure,
  minus 50 for the link.

**Installing.** Both paths, and when to choose which:

- Plain manifests from the release: `kuport-<version>.yaml`, image already pinned
  by digest. This is the default.
- The Helm chart: `kuport-<version>.tgz`, for when the `classes` value is easier
  than a second apply, or when the cluster's tooling installs charts anyway.

Pin by tag **and** digest. The digest decides; the tag is for people.

**Writing the PortMapClass.** This is where the integrating agent has to think,
so give it the reasoning rather than a template to paste. Walk one worked
example: a public class on the node that has the public address, with both the
public interface and the overlay interface listed so the port answers on the LAN
address and the overlay address at once; and an intranet class across the
workers with `returnPath.mode: None`, because a DaemonSet workload has a pod on
every accepting node and no return path is ever programmed.

Say which fields are decisions and which are defaults: `nodeSelector` and
`interfaces` are the cluster's shape and must be written; `ports`, and the vxlan
`vni`/`port`/`subnet` have defaults that are usually right; `namespaceSelector`
is a policy decision about who may ask.

**Letting a workload ask.** The namespace label the class selects on, and the
PortMap beside the Service. Note that the Service may be any type, ClusterIP
included, that it exists only so the agent has endpoints to follow and a named
port to resolve, and that kuport never modifies it.

**Verifying the install**, in order, with the observable at each step: the
DaemonSet has a pod on every selected node, the class reports `Ready=True` with
its `nodes[]` rows populated, a PortMap reports `Accepted=True` then
`Programmed=True`, `published` lists the addresses, and finally a probe from
outside shows the client's own address arriving at the pod. Be explicit that the
last step is the only one that proves the datapath, and that everything before
it can be green while the port stays shut — most often because of a host
firewall kuport does not touch.

**What kuport will not do**, so the agent does not go looking: no HTTP routing or
TLS, no load balancing across nodes, no IPv6, no admission webhooks, nothing in
the `filter` table, and no support for several accepting nodes forwarding to one
remote pod.

**Upgrades.** The agent version is semver and moves independently of the CRD
version. A CRD schema change needs `kubectl apply -f crds-<version>.yaml` by
hand, because Helm installs `crds/` once and never upgrades it — deliberately,
since an upgrade that dropped the CRD would delete every PortMap in the cluster.

`/home/nixos/repos/walzen-2-infra-test` is a **read-only** reference for the
shape of a consuming cluster's repo. You may read it to make the examples
concrete. Do not modify it, do not write cluster-specific credentials or
addresses into kuport's docs, and do not make kuport's documentation depend on
that repo existing — kuport ships the manifests and the image and knows nothing
about any particular cluster.

### 5. `README.md`

Update the **Status** section: it currently says "Design. Nothing is built."
Replace it with what is true — built, untested against a cluster, and what that
means for someone considering it. Add a short "Install" pointing at the release
artifacts and `docs/integration.md`. Keep the existing framing of the problem;
it is good and it does not need rewriting.

## Constraints

1. **Load `catalyst-v2-writing-docs` before your first edit** and follow it,
   humanizer pass included. This is the whole task; it is not background reading.
   Every file here is human-facing prose.
2. **The code is the authority on what exists.** Read it. Where a document and
   the code disagree, write what the code does and flag it in your report.
3. **Do not describe anything as tested against a cluster.** There is no test
   cluster. Unit and golden tests ran; the end-to-end script exists and has not
   been run. Say so where a reader would otherwise assume otherwise.
4. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag. The wave's
   meta-agent makes one commit per task once that task's gate passes, so
   parallel delegates never race one git index.
5. **Stay inside your Target.** Four files, no code.
6. **`/home/nixos/repos/walzen-2-infra-test` is read-only.**
7. **Toolchain is entered, never assumed** for any command you run.
8. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`**, never `/tmp`, never
   `.cortex/`.
9. **Acceptance criteria are inviolable.** If one cannot be met, stop and report
   it with the criterion intact.
10. **Report as a diff, not a commit**: files changed, `git diff --stat`,
    deviations.
11. **Emission discipline**: at most 20 minutes of reading before your first file
    write — you have more to read than most tasks — then a write every ~10
    minutes.

## Acceptance

From `/home/nixos/repos/kuport`. Paste verbatim output into your report.

```
nix shell nixpkgs#mermaid-cli -c bash -c 'for f in $(grep -l "^\`\`\`mermaid" docs/*.md); do echo "== $f"; done'
```

Then, for each mermaid block, extract it to `.tmp/` and render it, which is the
only way to know it parses:

```
nix shell nixpkgs#mermaid-cli -c mmdc -i .tmp/diagram-N.mmd -o .tmp/diagram-N.svg
```

Every block must render without error. A mermaid block with a syntax error shows
as a broken box on the rendering site and nobody notices until a reader does.

Also run:

```
nix shell nixpkgs#markdownlint-cli -c markdownlint docs/ README.md || true
grep -rn 'Nothing is built' README.md docs/     # must return nothing
grep -c 'mermaid' docs/spec.md                   # must be 5 or more
```

**Negative checks**, both required:

- **No claim of cluster testing.** `grep -rniE 'tested (on|against) (a )?(live |real )?cluster|end.to.end (test )?(passed|ran)' docs/ README.md` returns nothing, or returns only sentences that explicitly say it did **not** happen. The spec's existing statements about the datapath being *proven by hand* on 2026-09-06 are true and stay; a claim that the *built software* was tested on a cluster would be false.
- **Every condition reason is documented.** Extract the reason constants from
  `internal/api/v1alpha1/conditions.go` and confirm each appears in
  `docs/operations.md`. Paste the comparison. A reason a user can see in
  `kubectl describe` and cannot find in the docs is the gap this catches.

Green is: every mermaid block rendering, both greps behaving as described, and
the condition-reason comparison showing full coverage.

## Report to

Your dispatch brief names the wave's meta-agent. Send your completion hand-back
to it with `c2d steer --agent <meta> "A2A: ..."`, not to the orchestrator.
