# Metrics and Grafana Dashboard Implementation Plan

> Built by the main session (the admin's decision on 2026-10-13: no subagents,
> no blanket reviewer). Each task ends with its own check; the next task starts
> only after the check passes. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Each kuport agent serves Prometheus metrics on its node's overlay address, and a Grafana board in the infra repo shows health, per-mapping traffic, return links and egress.

**Architecture:** Metrics read what kuport already programs. The single addition to the ruleset is the kup-count chain, which counts both directions of each DNAT'd flow and changes no verdict. The reconcile reports, beside an unchanged State, which PortMap or Egress each DNAT and egress entry serves. The agent folds live counters into in-memory totals before every Apply, because Apply replaces the table. A collector in internal/metrics parses the live rules, maps them to their owners, adds conntrack open-flow counts, Link identities and apply state, and serves them from listeners on the InternalIP or a named interface. The infra repo adds a Talos firewall rule, a PodMonitor, a kube-state-metrics CustomResourceState block and the dashboard JSON.

**Tech Stack:** Go via `nix develop --command`, google/nftables v0.3.0, vishvananda/netlink v1.3.1, prometheus/client_golang v1.24.0, controller-runtime metrics.Registry; OpenTofu; Grafana 13.2.3.

**Spec:** .cortex/plans/2026-10-13-metrics/design.md

## Architecture brief

Read before any task. Each fact broke something once or will if ignored.

| Fact | Where | What goes wrong without it |
| --- | --- | --- |
| Apply deletes and re-adds `table inet kuport` in one transaction every pass | internal/datapath/nft.go `applyNFT` | every counter restarts at zero each pass |
| A pass runs on any watched change in the cluster, EndpointSlices included | internal/agent/agent.go `SetupWithManager` | assuming applies are rare |
| kup-steer runs at -120, between netbird's mark at -150 and Talos's firewall at -110; v0.7.0 broke every wt0 mapping by moving it | docs/datapath.md "Host firewall" | a new or moved prerouting chain breaks wt0 mappings |
| Cilium's pref 9 rule matches `mark & 0xf00 == 0x200`; kuport marks keep bits 8-11 clear | internal/reconcile/links.go `linkMark` | a new mark lands in Cilium's proxy table |
| netbird owns prefs 105 and 110; an Egress's rules sit at 32763-32765 | docs/datapath.md "Routing rule order" | a rule above netbird's takes netbird and LAN routes |
| Conntrack records the dialed address at -200, before kup-steer rewrites it to a stand-in | docs/datapath.md "Host firewall" | a counter keyed on the packet's destination sees the stand-in |
| On the pod's node, a kup-pre rule's `ip daddr` is the stand-in of the link end, written by Render | internal/datapath/render.go `Render` | matching a live kup-pre rule by its DstAddr fails; match by interface, port and DNAT target |
| `ct … saddr` with a direction loads 16 bytes in an inet table, an IPv4 address padded with 12 zero bytes; google/nftables v0.3.0 sends a direction only with the generic key CtKeySRC | spec "Reply counter" | the reply counter compiles and never matches |
| The agent runs with hostNetwork; a wildcard bind listens on a public uplink | deploy/daemonset.yaml | /metrics on the internet |
| Goldens compare nftText, not the applied expressions; only the kernel tests (run under `unshare -rn`) prove behaviour | internal/datapath/text_test.go | a passing golden taken as proof of a working rule |
| google/nftables lists a chain's rules in insertion order with their expressions and counters; a counter is `*expr.Counter{Packets, Bytes}` | rule.go `GetRules` | |
| netlink v1.3.1 `ConntrackFlow` has Forward/Reverse `IPTuple` and no status field | conntrack_linux.go | filtering on a status that is not there |

## Global constraints

- Existing rules, chains, priorities and State stay as they are. `git diff` of every existing golden shows only added lines inside `chain kup-count`.
- The agent never binds a wildcard. Default bind `$HOST_IP`; `--metrics-bind` takes an interface name or `off`; `--metrics-port` defaults to 9790. Plain HTTP.
- A missing bind address or failed bind never stops reconciling; retry every 30 s, log once per failure streak.
- Series and labels as the spec's Metrics table. `role` is `deliver` when the rule's client-side interface starts with `kup-`, else `accept`. `direction` is `request` or `reply`.
- Conntrack dump capped at 5 s; a failure increments `kuport_conntrack_dump_errors_total` and omits open flows from that scrape.
- Release v0.9.0. The dashboard lives in the infra repo. Infra: the admin applies talos and kube-prometheus; the kuport bump follows CLAUDE.md's per-session rule; infra commits need the permission sentence.

## Review focus

1. A remote mapping counted on both nodes: `role="accept"` on the accepting node and `role="deliver"` on the pod's node; summing accept counts the client's traffic once. Task 2's kernel test asserts both.
2. Twenty applies between two scrapes: totals keep every round. Task 2's kernel test applies between rounds.
3. wt0 without an address at agent start: datapath reconciles, listener binds later. Task 6's retry test.
4. An unrelated DNAT flow on the same port to another target: kup-count does not count it. Task 1's kernel test.
5. A port-range mapping: kup-count and the conntrack matcher take any port in the range. Tasks 1 and 3.

---

### Task 1: kup-count chain

**Files:** internal/datapath/render.go, nft.go, text.go; tests text_test.go, render_test.go, new metrics_kernel_test.go; goldens.

**Produces:** `ChainCount = "kup-count"`, `KindCount`, `Rule.CountTarget netip.Addr` (CtDir carries the direction; requests match `IifName`, replies `OifName`).

- [ ] Text tests first (TestNFTText cases for a request rule, an IPv6 reply rule over a range; TestTableHeaders gains the kup-count header). Run, see them fail on the unknown names.
- [ ] Render: chain `{ChainCount, "type filter hook postrouting priority srcnat + 10; policy accept;"}`, chainOrder 6; per DNAT rule append

  ```go
  Rule{Chain: ChainCount, Kind: KindCount, IifName: d.Iface, CtDir: CtDirOriginal, Port: d.Port, CountTarget: d.ToAddr},
  Rule{Chain: ChainCount, Kind: KindCount, OifName: d.Iface, CtDir: CtDirReply, Port: d.Port, CountTarget: d.ToAddr},
  ```

  `family()` returns `familyOf(r.CountTarget)` for KindCount; `dstKey` returns the target.
- [ ] nft.go: kup-count chain object (filter, postrouting, `ChainPriorityNATSource + 10`); `countExprs`: l4proto, ct direction, `ct status & 0x20 != 0`, `CtKeyPROTODST` direction original against the port or range, `CtKeySRC` direction reply against the 16-byte padded target, counter. Keep the leading nfproto match nftExprs adds.
- [ ] text.go: `[iifname|oifname] meta l4proto P ct direction D ct status dnat ct original proto-dst PORT ct reply ip[6] saddr T counter`; no leading `meta nfproto` for KindCount.
- [ ] Kernel test `TestCountsMappedTrafficOnRealKernel` (client, node, pod netns from the firewall test's helpers): mapping UDP 9987 to the pod, and a second DNAT on UDP 9999 to the pod gateway for the unrelated flow. Send 5 exchanges; read kup-count with GetRules; request and reply counters read 5 each; the unrelated rule pair reads 0 for traffic to 9987 and counts its own flow only.
- [ ] Mutation: compare a 4-byte target in countExprs, rerun, the reply counter reads 0; restore.

**Check:**

```sh
nix develop --command go test ./internal/datapath -run 'TestNFTText|TestTableHeaders|TestGolden' -update
git diff -U0 internal/datapath/testdata | grep '^[-+]' | grep -v '^+++\|^---' | grep -v 'kup-count\|ct original proto-dst\|^+\s*}$'
for f in internal/datapath/testdata/*.golden; do unshare -rn nft -c -f "$f" || echo "FAIL $f"; done
nix develop --command go test ./... -run 'RealKernel' -v 2>&1 | grep -E '^(=== RUN|--- (PASS|FAIL|SKIP))'
```

Pass: the second command prints nothing (no removed or changed line); no FAIL; TestDeliversThroughHostFirewallOnRealKernel, TestEgressOnRealKernel and the new test PASS without edits to the first two.

---

### Task 2: Live counters and totals across applies

**Files:** new internal/datapath/counters.go, counters_test.go; internal/agent/shutdown.go (HostDatapath); extend metrics_kernel_test.go.

**Produces:**

```go
// LiveRule is what the collector needs from one live rule.
type LiveRule struct {
	Chain    string
	Iif, Oif string
	Family   Family
	Proto    string // "tcp", "udp", "" for none
	Port     PortSel
	Dir      CtDirection
	Target   netip.Addr // kup-pre: DNAT target; kup-count: ct reply saddr
	Src      netip.Addr // kup-egress: ip saddr
	Mark     uint32     // kup-egress: mark set
	Counter  CounterValue
}
type CounterValue struct{ Packets, Bytes uint64 }
type CounterReader interface{ GetRules(*nftables.Table, *nftables.Chain) ([]*nftables.Rule, error) }
func ReadLive(r CounterReader) ([]LiveRule, error)            // chains kup-pre, kup-count, kup-egress, kup-mss
func (l LiveRule) Key() LiveKey                                 // every field but Counter
type Totals struct{ … }
func (t *Totals) Fold(live []LiveRule)
func (t *Totals) Snapshot(live []LiveRule) map[LiveKey]CounterValue
```

- [ ] Unit test `TestParseLiveRule`: build each rule kind with `nftExprs` (a kup-pre DNAT with a port range, both kup-count directions in IPv4 and IPv6, a kup-egress link mark and a blocked mark, kup-mss), wrap in `nftables.Rule{Exprs: …}` with a `*expr.Counter{Packets: 3, Bytes: 30}`, parse; every field equals the Rule it came from. This pins parse and render together.
- [ ] Unit test `TestTotalsSurviveApplies`: fold 3/300, fold 2/200, snapshot with live 1/100 gives 6/600; a key only in totals keeps its value; a key only in live starts at its live value.
- [ ] Implement the parser by walking expressions and remembering the last load (Meta key, Payload base and offset, Ct key and direction, Immediate register), then reading the following Cmp, Range or NAT.
- [ ] HostDatapath gains `Totals *datapath.Totals` and `CounterReader datapath.CounterReader`; Apply reads live, folds, then calls `datapath.Apply` unchanged. A failed read skips the fold.
- [ ] Kernel test extension: 5 exchanges, fold and Apply the same plan, 3 exchanges, snapshot: request and reply 8 each.

**Check:**

```sh
nix develop --command go test ./internal/datapath ./internal/agent
git diff --stat internal/datapath/apply.go internal/datapath/nft.go internal/datapath/state.go
```

Pass: tests PASS; the diff stat lists no line in apply.go or state.go, and nft.go only from Task 1.

---

### Task 3: Open flows from conntrack

**Files:** new internal/datapath/conntrack.go, conntrack_test.go.

**Produces:**

```go
type DNATKey struct {
	Iface, Proto string
	First, Last  uint16
	Target       netip.Addr
}
func KeyOf(d DNATRule) DNATKey
func CountFlows(flows []*netlink.ConntrackFlow, dnat []DNATRule) map[DNATKey]int
func DumpFlows(ctx context.Context, list func() ([]*netlink.ConntrackFlow, error)) ([]*netlink.ConntrackFlow, error) // 5 s cap
func HostConntrack() ([]*netlink.ConntrackFlow, error) // both families
```

A flow matches a DNAT rule when its protocol and original destination port fall in the rule's selection and its reply source equals the rule's target, and the reply source differs from the original destination. Among matching rules, one with a DstAddr equal to the flow's original destination wins (a link rule); otherwise the first without DstAddr.

- [ ] `TestCountFlows`: a range rule on enp1s0 and a kup-0 rule for the same pod; flows accepted, delivered, out of range and untranslated; counts 1 and 1.
- [ ] `BenchmarkCountFlows50k`: 50 000 flows, 20 rules; record ns/op in the commit message.

**Check:** `nix develop --command go test ./internal/datapath -run TestCountFlows` PASS; benchmark under 1 s per op.

---

### Task 4: Ownership index from the reconcile

**Files:** new internal/reconcile/metrics.go, metrics_test.go; reconcile.go gains `Result.Metrics` and one line in Compute.

**Produces:**

```go
type Owner struct{ Namespace, Name string }
type EgressOwner struct {
	Namespace, Egress, Pod string
	Blocked                bool
}
type MappingInfo struct{ Namespace, Name, Class, Mode, Protocol, Port, EndpointNode string }
type MetricsIndex struct {
	DNAT     map[datapath.DNATKey]Owner
	Egress   map[netip.Addr]EgressOwner
	Mappings []MappingInfo // one per mapping with a DNAT rule on this node
}
func buildMetricsIndex(in Inputs, idx *index, mappings []*mapping, egresses []*egressDecision, alloc linkAlloc) MetricsIndex
```

buildMetricsIndex runs `emitMapping` per mapping and `emitEgressPodNode` per pod on this node into a scratch `datapath.State`, and records each resulting DNAT entry's key and each Egress entry's SrcAddr against its owner. It calls the existing emit functions unchanged; buildState is untouched.

- [ ] Tests: an accepting node with a local mapping and a remote mapping; the pod's node of the remote mapping; an Egress pod on the link path and a blocked one. Each DNAT key in `res.State.DNAT` has an owner in the index; each `res.State.Egress` SrcAddr has an EgressOwner with the right Blocked; Mappings rows carry class, mode, protocol, port (`27015-27115` for a range) and endpoint node.
- [ ] A regression guard: for every existing reconcile test fixture, `Compute` returns the same `State` as before (the tests already assert State; they must pass without edits).

**Check:**

```sh
nix develop --command go test ./internal/reconcile
git diff internal/reconcile/reconcile.go internal/reconcile/egress.go | grep '^[-+]' | grep -v '^+++\|^---'
```

Pass: PASS; the diff shows only the Result field, its comment and the one Compute line.

---

### Task 5: Collector and apply metrics

**Files:** new internal/metrics/collector.go, collector_test.go; internal/agent/agent.go.

**Produces:**

```go
type LinkInfo struct{ Device, Slot, Class, Peer string }
type Snapshot struct {
	Index reconcile.MetricsIndex
	DNAT  []datapath.DNATRule
	Links []LinkInfo
}
type Source interface {
	Snapshot() Snapshot
	Counters() (map[datapath.LiveKey]datapath.CounterValue, error)
	Flows(ctx context.Context) ([]*netlink.ConntrackFlow, error)
}
func New(src Source) *Collector
var ApplyTotal *prometheus.CounterVec   // kuport_apply_total{result}
var LastApplySuccess prometheus.Gauge
var StateEntries *prometheus.GaugeVec   // {kind}
var ConntrackDumpErrors prometheus.Counter
```

Mapping a live rule to an owner: kup-count and kup-pre rules by `DNATKey{Iface: Iif or Oif, Proto, First, Last, Target}`; kup-egress by Src; kup-mss has none. A live rule with no owner is skipped (a table from a pass the index does not describe yet).

- [ ] `testutil.CollectAndCompare` per family: packets/bytes for accept request and reply, deliver on `kup-0`, connections from kup-pre, open flows, egress link and blocked, mss, link info, mapping info; a Flows error omits open flows and increments the dump-error counter.
- [ ] Agent: Reconciler gains `Totals`, `CounterReader`, `snap atomic.Pointer[metrics.Snapshot]`; implements Source; stores the snapshot after a successful apply (Links from `in.Links` whose Spec.Peers holds this node, device `kup-<n>` from the name `slot-<n>`); applyState records ApplyTotal, LastApplySuccess, StateEntries.
- [ ] Agent test: one reconcile with the fake Datapath; ApplyTotal ok is 1; the snapshot holds the fixture mapping.

**Check:** `nix develop --command go test ./internal/metrics ./internal/agent` PASS; `git diff internal/agent/agent.go` adds fields, the Source methods and lines in applyState and Reconcile only.

---

### Task 6: Endpoint, flags and manifests

**Files:** new internal/metrics/listen.go, listen_test.go; cmd/kuport-agent/main.go; deploy/daemonset.yaml; chart/templates/daemonset.yaml; chart/values.yaml.

**Produces:** `BindConfig{Bind, HostIP string; Port uint16}`, `ResolveAddrs(BindConfig, func(string) ([]netip.Addr, error)) ([]netip.AddrPort, error)`, `Server{Config, Lookup, Handler, Log, Retry}` as a manager.Runnable, `HostAddrs`.

- [ ] `TestResolveAddrs`: HOST_IP default; `wt0` gives its global IPv4 and IPv6; `off` gives none; empty HOST_IP, unknown interface and `0.0.0.0` are errors. A loop asserts no result is ever unspecified.
- [ ] `TestServerRetriesUntilAddressAppears`: lookup fails twice then returns 127.0.0.1; `/metrics` answers 200 within a second.
- [ ] Flags `--metrics-bind` (METRICS_BIND), `--metrics-port` (METRICS_PORT, 9790), HOST_IP from env; register the collector on `ctrlmetrics.Registry`; add the Server; controller-runtime's own metrics server stays `BindAddress: "0"`. Wire Totals and the CounterReader (`Handles.Counters()`) into both HostDatapath and the Reconciler.
- [ ] DaemonSet: HOST_IP from `status.hostIP`; container port `metrics` 9790. Chart: `metrics.bind`, `metrics.port`, `linkPort`, rendered as METRICS_BIND, METRICS_PORT, LINK_PORT.

**Check:**

```sh
nix develop --command go test ./...
nix develop --command helm lint chart
nix develop --command helm template chart | grep -E 'HOST_IP|METRICS_BIND|METRICS_PORT|LINK_PORT|containerPort: 9790'
```

Pass: PASS, lint clean, all five strings present.

---

### Task 7: Docs and release

- [ ] Load catalyst-v2-writing-docs. docs/metrics.md: bind rules, every series, `role` and `direction` with the two-node example, totals across applies, and a scrape pasted from the kernel test's collector output. docs/datapath.md: kup-count in both rule listings. docs/api.md: the flags and HOST_IP. README: one line to docs/metrics.md. design.md: "Changes made while building" table.
- [ ] Final grep from the skill over each doc.
- [ ] Push master; CI green; tag v0.9.0; release green.

**Check:** `curl -fsSL https://github.com/walzen-group/kuport/releases/download/v0.9.0/kuport-0.9.0.yaml | grep -cE 'HOST_IP|containerPort: 9790'` prints 2.

---

### Task 8: Infra (admin applies, permission sentence per commit)

- [ ] modules/infra/talos/opentofu/patches/firewall.tf: `kuport_metrics_port = 9790`; rule `kuport-metrics`, tcp, sources overlay and pod subnets. Plan the prod talos unit; show the admin.
- [ ] environments/prod/networking/kuport/inputs.yaml: `kuport_version: "v0.9.0"`; plan; show; apply per the session rule.
- [ ] modules/cluster/kube-prometheus: PodMonitor kuport modelled on `kubectl_manifest.backup_controller_pod_monitor` (selector `app.kubernetes.io/name: kuport`, port `metrics`, honorLabels, relabel `__meta_kubernetes_pod_node_name` to `node`); kube-state-metrics customResourceState for PortMap conditions, Egress pod paths and Links, with rbac.extraRules for kuport.wlz.li. Look up the CustomResourceState schema for the pinned chart version before writing it. Plan; show; the admin applies.
- [ ] flux/templates/management/grafana/base/dashboards/kuport.json and its kustomization entry: rows Health, Mappings (node graph plus table), Traffic, Return links, Egress, Agent, with the queries from the spec. Check the node graph renders before committing.
- [ ] modules/networking/kuport/opentofu/README.md: a Metrics section (doc skill first).

**Check:** after the admin's applies, `up{job="monitoring/kuport"}` returns six series at 1, and the board shows every PortMap from `kubectl get portmaps -A` in the Mappings table.
