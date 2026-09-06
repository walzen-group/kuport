# Task 2 — Datapath: nftables and netlink rendering and apply

Repo root: `/home/nixos/repos/kuport`. All paths relative to it.

## Context

kuport delivers a TCP or UDP port from one node's addresses to a pod on another
node, keeping the client's source address. Read `docs/spec.md` first, especially
the **Datapath** section. Every rule in it carried real traffic by hand on a live
cluster on 2026-09-06, and each one names the measurement that confirmed it. The
rules are not negotiable and not simplifiable; several of them exist to work
around undocumented behaviour in Cilium, WireGuard and the kernel.

You own `internal/datapath`, the **only package in the repo permitted to touch
the host**. Everything above you hands you a `State` struct and expects the host
to match it. Task 3 computes those structs and task 6 calls your `Apply`.

Task 1 has landed the Go module (`github.com/walzen-group/kuport`), the flake,
and the API types. There is no `go` on the host PATH; use `nix develop -c`.

## Target

Create:

- `internal/datapath/state.go` — the `State` struct **exactly** as given below.
  You own this file; tasks 3 and 6 import it and must not edit it.
- `internal/datapath/render.go` — `State` → `Plan`
- `internal/datapath/nft.go` — `Plan` → `github.com/google/nftables` objects
- `internal/datapath/netlink.go` — links, rules and routes via
  `github.com/vishvananda/netlink`
- `internal/datapath/apply.go` — `Apply` and `Teardown`
- `internal/datapath/text.go` — `Plan.String()`, the golden renderer
- `internal/datapath/render_test.go`, `internal/datapath/text_test.go`
- `internal/datapath/testdata/*.golden`

Do not touch: `internal/reconcile/`, `internal/agent/`, `cmd/`, `config/`,
`deploy/`, `chart/`, `docs/`, `README.md`, `flake.nix`, `go.mod` beyond adding
the two dependencies you need.

## Change

### 1. `state.go` — copy this verbatim

This struct is a pinned contract. Task 3 is being written against this exact
text in parallel with you. If you believe it needs a change, **stop and report
it**; do not change it.

```go
package datapath

import "net/netip"

// State is the complete desired host state for ONE node. The reconcile computes
// it; Apply makes the host match it. A mapping that was deleted disappears by
// being absent from a later State, never by anything remembering to remove it.
type State struct {
	DNAT   []DNATRule
	Exempt []ExemptRule
	Mark   []MarkRule
	Links  []Link
	Rules  []IPRule
	Routes []Route
}

// PortSel is a protocol and an inclusive port range. Last == First for a single
// port, which is the common case.
type PortSel struct {
	Proto string // "tcp" or "udp"
	First uint16
	Last  uint16
}

// DNATRule runs on an accepting node: one per class interface per mapping.
type DNATRule struct {
	Iface  string
	Port   PortSel
	ToAddr netip.Addr
}

// ExemptRule is an identity SNAT that claims the connection before the CNI's
// masquerade can. Negate renders `oifname != <OifName>`.
type ExemptRule struct {
	OifName   string
	Negate    bool
	DstAddr   *netip.Addr
	SrcAddr   *netip.Addr
	Port      PortSel
	PortIsSrc bool
}

// MarkRule runs on a target node, marking replies so they route back through
// the accepting node rather than out this node's own uplink.
type MarkRule struct {
	SrcAddr netip.Addr
	Port    PortSel // matched as source port
	Mark    uint32
}

// Link is one point-to-point VXLAN to a peer node.
type Link struct {
	Name       string // kup-<first 8 hex of sha256(peer node name)>
	VNI        uint32
	Port       uint16
	LocalAddr  netip.Addr
	RemoteAddr netip.Addr
	LinkAddr   netip.Prefix // this node's end, a /31
}

type IPRule struct {
	Pref  uint32
	Mark  uint32
	To    *netip.Prefix // set only on the loop-guard rule
	Table uint32
}

type Route struct {
	Table uint32
	Via   netip.Addr
	Dev   string
}
```

### 2. Naming and numbering — fixed

| Object | Value |
|---|---|
| nftables table | `kuport`, family `ip` |
| chains | `kup-pre`, `kup-post`, `kup-mangle` |
| `kup-pre` | `type nat hook prerouting priority dstnat - 10; policy accept;` |
| `kup-post` | `type nat hook postrouting priority srcnat - 10; policy accept;` |
| `kup-mangle` | `type filter hook prerouting priority mangle + 10; policy accept;` |

**`mark` and `fwd` are reserved words the nftables parser rejects as chain
names.** Both were found the hard way, one by a ruleset that had never run
before. Every chain name here is prefixed `kup-`; keep it that way.

### 3. Rendering

`Render(s State) Plan` produces a `Plan` holding the table, its three chains,
an ordered `[]Rule`, and the netlink objects. `Rule` is a small typed struct
with semantic fields (match interface, direction, protocol, port range, source
address, destination address, verdict/statement) — **not** raw
`[]expr.Any` and **not** a string.

Two consumers derive from that one struct:

- `nftExprs(r Rule) []expr.Any` in `nft.go`, what actually gets applied.
- `nftText(r Rule) string` in `text.go`, what the goldens compare.

This shape is deliberate. A golden that passes while the applied expressions are
wrong is the failure mode to guard against, so both derive from one description
rather than being written twice. Two things make that guard real, and both are
required:

- A test asserting `nftExprs` returns a non-empty slice for every `Rule` your
  renderer can produce, driven off the same table of cases as the goldens.
- Syntax-checking each golden with the real parser:
  `nix shell nixpkgs#nftables -c nft -c -f internal/datapath/testdata/<file>.golden`.
  `nft -c` parses without applying and needs no root. A golden that the real
  parser rejects is a bug in your renderer, not in the golden.

Ordering must be deterministic: sort rules within a chain by
(interface, protocol, first port, destination address). Two runs on the same
`State` produce byte-identical output.

### 4. The rules themselves

**Accepting node.** One DNAT per interface per mapping, in `kup-pre`:

```
iifname "<iface>" <proto> dport <port> counter dnat to <pod ip>:<port>
```

With a range, `dport <first>-<last>` and `dnat to <pod ip>:<first>-<last>`.

The masquerade exemption in `kup-post`:

```
oifname "cilium_host" ip daddr <pod ip> <proto> dport <port> counter snat to ip saddr
```

That is an identity SNAT, and its job is to claim the connection before Cilium
does. Cilium rewrites the source of anything entering the pod network through
`cilium_host` whose source is outside the node's pod CIDR — exactly a forwarded
packet. Netfilter sets up source NAT once per hook, so the rule that claims the
connection first wins. Measured: without the exemption the pod reported the
node's `cilium_host` address as the client; with it, the real client. The rule
carries the same `oifname` as the one it precedes so it claims only what that
rule would have claimed.

**Target node, when it is not the accepting node.** In `kup-post`:

```
oifname != "cilium_*" ip saddr <pod ip> <proto> sport <port> counter snat to ip saddr
```

Note the wildcard and the negation. Without this the reply's source is rewritten
to the node's own address and it leaves that node's own internet connection;
measured, the reply left the target site entirely and the client's conntrack
discarded it.

In `kup-mangle`:

```
ip saddr <pod ip> <proto> sport <port> counter meta mark set <mark>
```

The mark is what keeps the diversion narrow. Measured: a rule selecting on the
pod's address instead of a mark broke that pod's internet egress completely
while leaving its in-cluster traffic alone, because Cilium's own routing rule at
pref 9 catches that first.

### 5. Links, rules and routes

```
ip link add <name> type vxlan id <vni> local <own address> remote <peer address> dstport <port> ttl 64
ip addr add <link address>/31 dev <name>
ip link set <name> up
```

The outer header carries the two nodes' addresses, which the WireGuard mesh
accepts, so no key material is involved and nothing needs renewing. Measured:
12ms across two sites on the first try. The dstport must differ from the CNI's,
8472 for Cilium.

GRE would be lighter, 24 bytes against 50, but Talos does not ship the module:
`ip link add type gre` answers `Unknown device type`. Do not switch to it.

Rules and routes, per link:

```
ip rule  add pref 101 fwmark <mark> to <accepting node address> lookup main
ip rule  add pref 102 fwmark <mark> lookup <table>
ip route add default via <accepting node link address> dev <link> table <table>
```

**Two ordering constraints, both load-bearing.**

The rules sit at pref 101 and 102 because netbird owns pref 110 and sends
everything without its mark to its own table, and pref 105 resolves the
destination out of `main` before that. A return rule at 32001 never fires; the
reply has already left. They must stay above 105 and below the local table
at 100. Linux permits duplicate preference values, so every link's rules share
101 and 102 and are told apart by their marks.

The rule at 101 prevents a routing loop and is not optional. The VXLAN driver
copies the packet's mark onto the encapsulated packet, whose destination is the
peer's address. That outer packet then matches the rule at 102 and is routed
back into the device it just came out of. The kernel refuses and increments the
device's `tx_errors` with no log line anywhere. Measured: one transmit error per
probe, and the reply never left.

### 6. Apply and Teardown

`Apply(ctx, plan) error`:

- nftables: **one transaction**. Add the table, flush it, add chains and rules,
  then `Flush()`. Rewriting the whole table each pass is what makes a deleted
  mapping disappear by absence.
- netlink has no transaction, so links, rules and routes are reconciled by
  comparison. Read what exists, add what is missing, remove what the agent finds
  **under its own names** and does not want. Never touch an object kuport did
  not name: the table `kuport`, links matching `kup-*`, routing tables in the
  200–327 range that appear in the plan, and rules carrying a `0x6b70****` mark.
- Idempotent. Applying the same plan twice makes no change on the second pass,
  and a test must show that.

`Teardown(ctx) error` removes the table, the `kup-*` links, and kuport's rules
and routes, so a node taken out of a class stops holding state nobody wants. A
crashed agent leaves them and the next `Apply` reconciles them away.

Both take an interface for the netlink and nftables handles so the pure paths
stay testable without root. Do not require root to run `go test`.

### 7. MTU

Expose `func LinkMTU(underlay int) int { return underlay - 50 }` and a helper
that reads an interface's MTU through netlink. Task 6 reports both into
`PortMapClass.status.nodes[]`. The 50 bytes are the VXLAN overhead; on the
cluster this was designed against the margin is exactly zero, which is why an
operator needs to see the number.

## Constraints

1. **Toolchain is entered, never assumed.** No `go` on the host PATH. Every Go
   command runs as `nix develop -c <cmd>` from the repo root.
2. **Committing is the meta-agent's job.** Leave your changes uncommitted;
   `git add` is fine. No `git commit`, no branch, no push, no tag. The wave's
   meta-agent makes one commit per task once that task's gate passes, so
   parallel delegates never race one git index.
3. **Stay inside your Target.** `state.go` is yours; the rest of
   `internal/datapath` is yours; nothing else is.
4. **Never shell out to `nft` or `ip` in product code.** Speak netlink directly.
   The predecessor shelled out and installed iproute2 from the Alpine mirror at
   pod start, so a node rebooting while the mirror was unreachable came up with
   no rules and the forward stayed down until the install worked. The image is a
   static binary on scratch with no package manager. `nft -c` in a *test* is
   fine; it is a syntax checker, not the datapath.
5. **`internal/datapath` is the only package that touches the host**, and within
   it only `nft.go`, `netlink.go` and `apply.go` do. `render.go`, `text.go` and
   `state.go` are pure.
6. **Look up current versions before pinning.** `github.com/google/nftables` and
   `github.com/vishvananda/netlink` both move; check the current release rather
   than writing one from memory.
7. **Scratch goes to `/home/nixos/repos/kuport/.tmp/`**, never `/tmp`, never
   `.cortex/`.
8. **Human-facing prose follows `catalyst-v2-writing-docs`**, humanizer pass
   included, for any package doc comment that runs to prose.
9. **Acceptance criteria are inviolable.** If one cannot be met, stop and report
   it with the criterion intact.
10. **Report as a diff, not a commit**: files changed, `git diff --stat`,
    verbatim gate output, deviations.
11. **Emission discipline**: at most 15 minutes of survey before your first file
    write, then a write every ~10 minutes.

## Acceptance

From `/home/nixos/repos/kuport`. Paste verbatim output of each into your report.

```
nix develop -c go build ./...
nix develop -c go vet ./...
nix develop -c go test ./internal/datapath/... -v
nix develop -c golangci-lint run ./internal/datapath/...
```

Golden files must exist and be syntax-clean against the real parser:

```
for f in internal/datapath/testdata/*.golden; do nix shell nixpkgs#nftables -c nft -c -f "$f" && echo "OK $f"; done
```

The required golden cases, at minimum:

| Case | Node | Expect |
|---|---|---|
| `accepting-single-port` | `edge-a`, ifaces `enp1s0` + `wt0`, UDP 3000 → `10.244.18.107` | two DNAT rules, one exemption |
| `accepting-port-range` | same, UDP 27015–27115 | `dport 27015-27115`, `dnat to <ip>:27015-27115` |
| `target-remote` | `worker-b` holding the pod, accepting node `edge-a` | mark rule, reply exemption, two ip rules, one route, one link |
| `same-node` | pod on the accepting node | DNAT and exemption only, **no** link, **no** mark, **no** rules |
| `empty` | no mappings | table and three chains present, zero rules |

**The `same-node` case is a real correctness check, not filler.** A packet is
destination-NATed once per netfilter hook, so an accepting node that also holds
the pod must DNAT straight to the pod address with no return-path machinery.

**Negative checks**, both required:

- Idempotence: apply the same `Plan` twice against a fake handle and assert the
  second pass issues no change. A test that only applies once proves nothing.
- Determinism: render the same `State` twice with map iteration in play and
  assert byte-identical output. Run it with `-count=10`.

Green is: build and vet silent, every `go test` case `ok`, `golangci-lint`
clean, every golden accepted by `nft -c`.

## Report to

Your dispatch brief names the wave's meta-agent. Send your completion hand-back
to it with `c2d steer --agent <meta> "A2A: ..."`, not to the orchestrator.
