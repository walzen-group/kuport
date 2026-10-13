# Metrics and Grafana Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each kuport agent serves Prometheus metrics on its node's overlay address, and a Grafana board in the infra repo shows health, per-mapping traffic, return links and egress.

**Architecture:** Every rule a mapping or Egress owns carries an nftables comment naming its object. A new chain kup-count counts both directions of each DNAT'd flow. Because Apply replaces the whole table each pass, the agent folds the live counters into in-memory totals before every Apply. A Prometheus collector in a new internal/metrics package turns the totals, a conntrack dump, the agent's Links and its apply state into series, served by the agent's own listeners on the InternalIP (or a named interface). The infra repo adds a Talos firewall rule, a PodMonitor, a kube-state-metrics CustomResourceState block and the dashboard JSON.

**Tech Stack:** Go 1.x (nix develop), google/nftables v0.3.0 (expr, userdata), vishvananda/netlink v1.3.1 (conntrack), prometheus/client_golang v1.24.0, controller-runtime metrics.Registry; OpenTofu (talos, kube-prometheus modules); Grafana 13.2.3 dashboard JSON.

**Spec:** .cortex/plans/2026-10-13-metrics/design.md

## Global Constraints

- The agent never binds a wildcard address. Default bind: `$HOST_IP` (status.hostIP). `--metrics-bind` takes an interface name or `off`. `--metrics-port` defaults to 9790.
- Plain HTTP, no authentication on /metrics.
- A missing bind address or failed bind never stops reconciling; the listener retries every 30 seconds and logs once per failure streak.
- Every rule kuport renders for a PortMap carries `comment "<namespace>/<name>"`; for an Egress `comment "egress:<namespace>/<egress>/<pod>"`.
- kup-count: `type filter hook postrouting priority srcnat + 10; policy accept;`, two rules per DNAT rule, no verdict.
- Series and labels exactly as the spec's Metrics table; traffic series carry `role` (`accept` on the DNAT rule's client interface, `deliver` when the interface starts with `kup-`) and `direction` (`request`, `reply`).
- Counters survive Apply: fold live counters into totals before each Apply.
- Conntrack dump capped at 5 seconds; failure increments `kuport_conntrack_dump_errors_total` and omits `kuport_mapping_open_flows` from that scrape.
- Release v0.9.0. The dashboard lives in the infra repo at flux/templates/management/grafana/base/dashboards/kuport.json; kuport ships none.
- Infra: the talos and kube-prometheus units are applied by the admin. The kuport version bump follows CLAUDE.md's per-session rule. Infra commits need the admin's permission sentence.
- kuport repo: commit and push to master freely; tag v0.9.0 when CI is green.
- Run Go through `nix develop --command`.

## Review Focus

1. A remote mapping counted on both nodes: summing `role="accept"` must equal the client's traffic once; the kernel test asserts the accepting node's accept series and the pod node's deliver series separately.
2. An agent applying 20 times between two scrapes: totals keep every round. Covered by the counters-across-applies kernel test.
3. wt0 absent at agent start (netbird not up yet): the datapath reconciles and the listener binds once wt0 gets its address. Covered by the bind test with an injected address lookup that fails, then succeeds.
4. An unrelated DNAT flow on the same port (kube-proxy NodePort or a second mapping's target): kup-count must not count it. Covered by the kernel test's unrelated-flow case.
5. A PortMap with a port range (`27015-27115`): kup-count and the conntrack matcher accept any port in the range. Covered in Task 2's text test and Task 4's matcher test.

---

## File structure

| File | Responsibility |
| --- | --- |
| internal/datapath/state.go | `Owner` on DNATRule and EgressRule |
| internal/datapath/render.go | `Comment` and count fields on Rule; ChainCount; render kup-count; carry comments |
| internal/datapath/nft.go | userdata comment on each rule; kup-count chain; count expressions |
| internal/datapath/text.go | `comment "…"` and the count rule text |
| internal/datapath/counters.go (new) | read live counters from the kernel; fold totals across applies |
| internal/datapath/conntrack.go (new) | count open flows per DNAT rule from a conntrack dump |
| internal/datapath/metrics_kernel_test.go (new) | kup-count and counters-across-applies on a real kernel |
| internal/reconcile/reconcile.go, egress.go | set Owner; `Result.Mappings` info rows |
| internal/metrics/collector.go (new) | the Prometheus collector |
| internal/metrics/listen.go (new) | resolve bind addresses, serve, retry |
| internal/agent/agent.go, shutdown.go | fold counters before Apply; record apply metrics; hand state to the collector |
| cmd/kuport-agent/main.go | flags, wiring |
| deploy/daemonset.yaml, chart/templates/daemonset.yaml, chart/values.yaml | HOST_IP, METRICS_*, LINK_PORT, port metrics |
| docs/metrics.md (new), docs/api.md, docs/datapath.md | series reference, flags, kup-count |
| infra: modules/infra/talos/opentofu/patches/firewall.tf | rule kuport-metrics |
| infra: modules/cluster/kube-prometheus/opentofu/manifests.tf, main.tf, README.md | PodMonitor kuport, kube-state-metrics CustomResourceState |
| infra: flux/templates/management/grafana/base/dashboards/kuport.json, kustomization | the board |
| infra: modules/networking/kuport/opentofu/README.md | metrics section |

---

### Task 1: Owner comments on every mapping and Egress rule

**Files:**
- Modify: `internal/datapath/state.go` (DNATRule, EgressRule)
- Modify: `internal/datapath/render.go` (Rule.Comment; Render copies Owner)
- Modify: `internal/datapath/nft.go` (applyNFT sets UserData)
- Modify: `internal/datapath/text.go` (nftText appends comment)
- Modify: `internal/reconcile/reconcile.go:376`, `:463`; `internal/reconcile/egress.go:277,285,341,345`
- Test: `internal/datapath/text_test.go` (TestNFTText), goldens, `internal/reconcile/reconcile_test.go`, `internal/datapath/render_test.go`

**Interfaces:**
- Produces: `DNATRule.Owner string`, `EgressRule.Owner string`, `Rule.Comment string`, `func ownerOfPortMap(pm *v1alpha1.PortMap) string`, `func ownerOfEgress(eg *v1alpha1.Egress, pod string) string`.

- [ ] **Step 1: Write the failing text test**

Add to the table in `TestNFTText`:

```go
{
	name: "dnat with owner comment",
	rule: Rule{Chain: ChainPre, Kind: KindDNAT, IifName: "enp1s0",
		Port: PortSel{Proto: "tcp", First: 10002, Last: 10002},
		ToAddr: netip.MustParseAddr("10.244.18.186"), Comment: "jellyfin/jellyfin"},
	want: `meta nfproto ipv4 iifname "enp1s0" tcp dport 10002 counter dnat ip to 10.244.18.186:10002 comment "jellyfin/jellyfin"`,
},
```

- [ ] **Step 2: Run it and see it fail**

Run: `nix develop --command go test ./internal/datapath -run TestNFTText`
Expected: FAIL, `unknown field Comment in struct literal`.

- [ ] **Step 3: Add the fields and render them**

state.go, in DNATRule and EgressRule:

```go
	// Owner names the object the rule serves, "<namespace>/<name>" for a
	// PortMap and "egress:<namespace>/<egress>/<pod>" for an Egress. It is
	// written as the rule's nftables comment, which the metrics collector reads.
	Owner string
```

render.go, in Rule after CtMark:

```go
	// Comment is the rule's nftables comment, the Owner of the State entry it
	// came from. Empty writes none.
	Comment string
```

In Render, set `Comment: d.Owner` on the DNAT rule, and `Comment: e.Owner` on the egress KindMark rule. Leave the steer rule's comment empty (one steer rule serves every mapping on its interface and port in that family).

text.go, at the end of nftText, replace each `return b.String()` path with a single exit that appends the comment:

```go
	if r.Comment != "" {
		fmt.Fprintf(&b, " comment %q", r.Comment)
	}
	return b.String()
```

Restructure the KindMSS early return to fall through to that exit.

nft.go, in applyNFT:

```go
	for _, r := range plan.Rules {
		nr := &nftables.Rule{Table: t, Chain: chainObjs[r.Chain], Exprs: nftExprs(r, sets)}
		if r.Comment != "" {
			nr.UserData = userdata.AppendString(nil, userdata.TypeComment, r.Comment)
		}
		conn.AddRule(nr)
	}
```

with the import `"github.com/google/nftables/userdata"`.

- [ ] **Step 4: Set Owner in the reconcile**

reconcile.go, a helper next to `mapping`:

```go
// ownerOfPortMap is the comment kuport writes on every rule a PortMap owns.
func ownerOfPortMap(pm *v1alpha1.PortMap) string { return pm.Namespace + "/" + pm.Name }
```

Set `Owner: ownerOfPortMap(m.pm)` on both `datapath.DNATRule{...}` literals (lines 376 and 463).

egress.go:

```go
// ownerOfEgress is the comment kuport writes on a selected pod's egress rule.
func ownerOfEgress(eg *v1alpha1.Egress, pod string) string {
	return "egress:" + eg.Namespace + "/" + eg.Name + "/" + pod
}
```

Set `Owner: ownerOfEgress(d.eg, p.name)` on the four EgressRule literals (lines 277, 285, 341, 345). keepState gets the same owners, so a shut-down agent's kept rules stay labelled.

- [ ] **Step 5: Reconcile test for Owner**

In reconcile_test.go, extend the existing single-mapping case (the one asserting the accepting DNAT) with:

```go
if got := res.State.DNAT[0].Owner; got != "udp-echo/udp-echo" {
	t.Errorf("DNAT owner = %q, want udp-echo/udp-echo", got)
}
```

and in egress_test.go's link-path case:

```go
if got := res.State.Egress[0].Owner; got != "egress:qbittorrent/qbittorrent/qb-0" {
	t.Errorf("egress owner = %q", got)
}
```

Use the namespace, PortMap, Egress and pod names those fixtures already create; read them from the fixture before writing the expected string.

- [ ] **Step 6: Regenerate goldens and check them**

Run: `nix develop --command go test ./internal/datapath -run TestGolden -update`
Then: `nix develop --command go test ./internal/datapath ./internal/reconcile`
Expected: PASS. `git diff internal/datapath/testdata` shows only ` comment "…"` appended to DNAT and egress lines.

Run each golden through nft in a user netns:

```sh
for f in internal/datapath/testdata/*.golden; do unshare -rn nft -c -f "$f" || echo "FAIL $f"; done
```

Expected: no FAIL line.

- [ ] **Step 7: Kernel round trip of the comment**

In netlink_kernel_test.go's `applyIPv6RealKernel`, after the apply, list the rules of kup-pre with `conn.GetRules` and assert at least one has `userdata.GetString(r.UserData, userdata.TypeComment)` equal to the fixture's owner. Run: `nix develop --command go test ./internal/datapath -run TestApplyIPv6OnRealKernel -v`. Expected: PASS (or SKIP where unshare is unavailable; CI runs it).

- [ ] **Step 8: Commit**

```bash
git add internal/datapath internal/reconcile
git commit -m "feat(datapath): name each mapping and egress rule's owner in an nftables comment"
```

---

### Task 2: kup-count chain

**Files:**
- Modify: `internal/datapath/render.go` (ChainCount, KindCount, Rule.CountTarget, render)
- Modify: `internal/datapath/nft.go` (chain, expressions)
- Modify: `internal/datapath/text.go`
- Test: `internal/datapath/text_test.go`, `render_test.go`, goldens

**Interfaces:**
- Consumes: `Rule.Comment` (Task 1).
- Produces: `ChainCount = "kup-count"`, `KindCount`, `Rule.CountTarget netip.Addr`; `Rule.CtDir` reused for direction. Count rules use `IifName` for requests and `OifName` for replies.

- [ ] **Step 1: Failing text tests**

```go
{
	name: "count request",
	rule: Rule{Chain: ChainCount, Kind: KindCount, IifName: "enp1s0", CtDir: CtDirOriginal,
		Port: PortSel{Proto: "tcp", First: 10002, Last: 10002},
		CountTarget: netip.MustParseAddr("10.244.18.186"), Comment: "jellyfin/jellyfin"},
	want: `iifname "enp1s0" meta l4proto tcp ct direction original ct status dnat ct original proto-dst 10002 ct reply ip saddr 10.244.18.186 counter comment "jellyfin/jellyfin"`,
},
{
	name: "count reply over a range, IPv6",
	rule: Rule{Chain: ChainCount, Kind: KindCount, OifName: "ens3", CtDir: CtDirReply,
		Port: PortSel{Proto: "udp", First: 27015, Last: 27115},
		CountTarget: netip.MustParseAddr("fd00::2"), Comment: "game/srv"},
	want: `oifname "ens3" meta l4proto udp ct direction reply ct status dnat ct original proto-dst 27015-27115 ct reply ip6 saddr fd00::2 counter comment "game/srv"`,
},
```

And in `TestTableHeaders` the expected header list gains `type filter hook postrouting priority srcnat + 10; policy accept;` for kup-count.

- [ ] **Step 2: Run, see it fail**

Run: `nix develop --command go test ./internal/datapath -run 'TestNFTText|TestTableHeaders'`
Expected: FAIL on the unknown names.

- [ ] **Step 3: Render**

render.go constants: `ChainCount = "kup-count"`; add to `chains` after ChainMSS:

```go
	{ChainCount, "type filter hook postrouting priority srcnat + 10; policy accept;"},
```

and `ChainCount: 6` in chainOrder. RuleKind: append `KindCount` with the comment "KindCount counts one direction of the flows a DNAT rule translates; it changes nothing." Rule field:

```go
	// CountTarget is a KindCount rule's DNAT target, matched as the reply
	// tuple's source: ct reply ip saddr.
	CountTarget netip.Addr
```

`Rule.family()`: add `case r.Kind == KindCount: return familyOf(r.CountTarget), true` before the SrcAddr case.

In Render, inside the `for _, d := range s.DNAT` loop after the DNAT rule:

```go
		rules = append(rules,
			Rule{Chain: ChainCount, Kind: KindCount, IifName: d.Iface, CtDir: CtDirOriginal,
				Port: d.Port, CountTarget: d.ToAddr, Comment: d.Owner},
			Rule{Chain: ChainCount, Kind: KindCount, OifName: d.Iface, CtDir: CtDirReply,
				Port: d.Port, CountTarget: d.ToAddr, Comment: d.Owner},
		)
```

dstKey: return `r.CountTarget.String()` for KindCount so two count rules on one interface sort by target.

- [ ] **Step 4: Expressions**

nft.go, nftChains gains:

```go
		{
			Name:     ChainCount,
			Table:    t,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookPostrouting,
			Priority: nftables.ChainPriorityRef(*nftables.ChainPriorityNATSource + 10),
			Policy:   &accept,
		},
```

In nftExprs, before the generic L4/port block:

```go
	if r.Kind == KindCount {
		return append(e, countExprs(r, fam)...)
	}
```

```go
// IPS_DST_NAT is the conntrack status bit a destination-translated flow carries.
const ipsDstNAT uint32 = 1 << 5

// countExprs matches one direction of a flow a DNAT rule translated: the
// protocol, the direction, the DNAT status bit, the port the client dialed
// and the DNAT target as the reply's source, then counts. The generic
// conntrack source key with a direction loads 16 bytes in an inet table, so
// an IPv4 target is compared padded with 12 zero bytes; google/nftables
// v0.3.0 sends a direction only with that key.
func countExprs(r Rule, fam Family) []expr.Any {
	dir := uint32(ipCtDirOriginal)
	if r.CtDir == CtDirReply {
		dir = uint32(ipCtDirReply)
	}
	target := make([]byte, 16)
	copy(target, addrBytes(r.CountTarget))
	out := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoNum(r.Port.Proto)}},
	}
	out = append(out, ctDirExprs(uint8(dir))...)
	out = append(out,
		&expr.Ct{Key: expr.CtKeySTATUS, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(ipsDstNAT), Xor: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Ct{Key: expr.CtKeyPROTODST, Register: 1, Direction: uint32(ipCtDirOriginal)},
	)
	if r.Port.First == r.Port.Last {
		out = append(out, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(r.Port.First)})
	} else {
		out = append(out, &expr.Range{Op: expr.CmpOpEq, Register: 1,
			FromData: binaryutil.BigEndian.PutUint16(r.Port.First), ToData: binaryutil.BigEndian.PutUint16(r.Port.Last)})
	}
	return append(out,
		&expr.Ct{Key: expr.CtKeySRC, Register: 1, Direction: uint32(ipCtDirReply)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: target},
		&expr.Counter{},
	)
}
```

The family match nftExprs prepends (meta nfproto) stays: it keeps an IPv4 count rule off IPv6 flows whose padded target could otherwise compare equal.

- [ ] **Step 5: Text**

nftText, right after the interface matches:

```go
	if r.Kind == KindCount {
		dir := "original"
		if r.CtDir == CtDirReply {
			dir = "reply"
		}
		fmt.Fprintf(&b, "meta l4proto %s ct direction %s ct status dnat ct original proto-dst %s ct reply %s saddr %s counter",
			r.Port.Proto, dir, portText(r.Port), l3Text(fam), r.CountTarget)
		if r.Comment != "" {
			fmt.Fprintf(&b, " comment %q", r.Comment)
		}
		return b.String()
	}
```

Skip the leading `meta nfproto` for KindCount: change the first condition to `if hasFam && r.SrcAddr == nil && r.DstAddr == nil && r.Kind != KindCount`, since `ct reply ip saddr` names the family.

- [ ] **Step 6: Run, regenerate, check**

```sh
nix develop --command go test ./internal/datapath -run 'TestNFTText|TestTableHeaders'
nix develop --command go test ./internal/datapath -run TestGolden -update
for f in internal/datapath/testdata/*.golden; do unshare -rn nft -c -f "$f" || echo "FAIL $f"; done
nix develop --command go test ./...
```

Expected: PASS; every golden with a DNAT gains a kup-count chain with two rules per DNAT; no FAIL line.

- [ ] **Step 7: Commit**

```bash
git add internal/datapath
git commit -m "feat(datapath): kup-count counts both directions of every mapped flow"
```

---

### Task 3: Read counters and keep totals across applies

**Files:**
- Create: `internal/datapath/counters.go`
- Create: `internal/datapath/counters_test.go`
- Create: `internal/datapath/metrics_kernel_test.go`
- Modify: `internal/agent/shutdown.go` (HostDatapath folds before Apply)

**Interfaces:**
- Produces:

```go
type CounterKey struct {
	Chain, Comment, Iif, Oif string
	Family                   Family
	Dir                      CtDirection
}
type CounterValue struct{ Packets, Bytes uint64 }
type CounterReader interface {
	GetRules(t *nftables.Table, c *nftables.Chain) ([]*nftables.Rule, error)
}
func ReadCounters(r CounterReader) (map[CounterKey]CounterValue, error)
type Totals struct{ /* mutex, folded map */ }
func (t *Totals) Fold(live map[CounterKey]CounterValue)
func (t *Totals) Snapshot(live map[CounterKey]CounterValue) map[CounterKey]CounterValue
```

- `HostDatapath` gains `Totals *datapath.Totals` and `CounterReader datapath.CounterReader`.

- [ ] **Step 1: Failing unit test for Totals**

```go
func TestTotalsSurviveApplies(t *testing.T) {
	k := CounterKey{Chain: ChainCount, Comment: "a/b", Iif: "enp1s0", Family: FamilyIPv4, Dir: CtDirOriginal}
	var tot Totals
	tot.Fold(map[CounterKey]CounterValue{k: {Packets: 3, Bytes: 300}}) // first Apply
	tot.Fold(map[CounterKey]CounterValue{k: {Packets: 2, Bytes: 200}}) // second Apply
	got := tot.Snapshot(map[CounterKey]CounterValue{k: {Packets: 1, Bytes: 100}})
	if got[k] != (CounterValue{Packets: 6, Bytes: 600}) {
		t.Fatalf("snapshot = %+v, want 6/600", got[k])
	}
	other := CounterKey{Chain: ChainCount, Comment: "c/d", Oif: "wt0", Dir: CtDirReply}
	got = tot.Snapshot(map[CounterKey]CounterValue{other: {Packets: 4, Bytes: 40}})
	if got[k] != (CounterValue{Packets: 5, Bytes: 500}) || got[other] != (CounterValue{Packets: 4, Bytes: 40}) {
		t.Fatalf("a key absent from the live table keeps its total, and a new key starts at its live value: %+v", got)
	}
}
```

- [ ] **Step 2: Run, see it fail**

Run: `nix develop --command go test ./internal/datapath -run TestTotalsSurviveApplies`
Expected: FAIL, `undefined: CounterKey`.

- [ ] **Step 3: Implement counters.go**

```go
package datapath

import (
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
)

// CounterKey names the series one rule feeds. Two rules with the same key add
// into one series.
type CounterKey struct {
	Chain, Comment, Iif, Oif string
	Family                   Family
	Dir                      CtDirection
}

// CounterValue is a rule's packet and byte count.
type CounterValue struct{ Packets, Bytes uint64 }

// CounterReader lists a chain's rules with their expressions and userdata.
// *nftables.Conn implements it.
type CounterReader interface {
	GetRules(t *nftables.Table, c *nftables.Chain) ([]*nftables.Rule, error)
}

// countedChains are the chains whose counters the collector reports.
var countedChains = []string{ChainPre, ChainCount, ChainEgress, ChainMSS}

// ReadCounters lists the live kuport table and returns each commented rule's
// counter, plus kup-mss's uncommented one. A missing table yields an empty map.
func ReadCounters(r CounterReader) (map[CounterKey]CounterValue, error) {
	out := map[CounterKey]CounterValue{}
	t := nftTable()
	for _, name := range countedChains {
		rules, err := r.GetRules(t, &nftables.Chain{Name: name, Table: t})
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, rule := range rules {
			k, v, ok := ruleCounter(name, rule)
			if !ok {
				continue
			}
			cur := out[k]
			out[k] = CounterValue{Packets: cur.Packets + v.Packets, Bytes: cur.Bytes + v.Bytes}
		}
	}
	return out, nil
}

// ruleCounter reads a rule's comment, interface matches, family, direction
// and counter back out of its expressions.
func ruleCounter(chain string, r *nftables.Rule) (CounterKey, CounterValue, bool) {
	k := CounterKey{Chain: chain}
	k.Comment, _ = userdata.GetString(r.UserData, userdata.TypeComment)
	if k.Comment == "" && chain != ChainMSS {
		return k, CounterValue{}, false
	}
	var v CounterValue
	var found bool
	var lastMeta expr.MetaKey
	for _, e := range r.Exprs {
		switch x := e.(type) {
		case *expr.Meta:
			lastMeta = x.Key
		case *expr.Ct:
			if x.Key == expr.CtKeyDIRECTION {
				lastMeta = 0xff
			}
		case *expr.Cmp:
			switch lastMeta {
			case expr.MetaKeyIIFNAME:
				k.Iif = trimNul(x.Data)
			case expr.MetaKeyOIFNAME:
				k.Oif = trimNul(x.Data)
			case expr.MetaKeyNFPROTO:
				if len(x.Data) == 1 && x.Data[0] == nfproto(FamilyIPv6) {
					k.Family = FamilyIPv6
				}
			case 0xff:
				if len(x.Data) == 1 && x.Data[0] == ipCtDirReply {
					k.Dir = CtDirReply
				} else {
					k.Dir = CtDirOriginal
				}
			}
			lastMeta = 0
		case *expr.Counter:
			v, found = CounterValue{Packets: x.Packets, Bytes: x.Bytes}, true
		}
	}
	return k, v, found
}

func trimNul(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Totals holds what earlier tables counted. Apply replaces the whole table,
// which restarts every counter, so the agent folds the live counters in here
// right before each Apply and reports totals plus the live table.
type Totals struct {
	mu     sync.Mutex
	folded map[CounterKey]CounterValue
}

// Fold adds a table's final counts to the totals.
func (t *Totals) Fold(live map[CounterKey]CounterValue) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.folded == nil {
		t.folded = map[CounterKey]CounterValue{}
	}
	for k, v := range live {
		c := t.folded[k]
		t.folded[k] = CounterValue{Packets: c.Packets + v.Packets, Bytes: c.Bytes + v.Bytes}
	}
}

// Snapshot returns the totals plus the live table's counts, every key from
// either side.
func (t *Totals) Snapshot(live map[CounterKey]CounterValue) map[CounterKey]CounterValue {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[CounterKey]CounterValue, len(t.folded)+len(live))
	for k, v := range t.folded {
		out[k] = v
	}
	for k, v := range live {
		c := out[k]
		out[k] = CounterValue{Packets: c.Packets + v.Packets, Bytes: c.Bytes + v.Bytes}
	}
	return out
}
```

`isNotExist(err)` is `errors.Is(err, unix.ENOENT)`; add it to counters.go if netlink.go has no such helper yet (check with `grep -n ENOENT internal/datapath/*.go` and reuse what exists).

The direction marker `0xff` is local to ruleCounter: a `ct direction` load followed by its compare. A direction compare on a KindCount rule is the only ct-direction match in kup-count; in kup-egress it is always original.

- [ ] **Step 4: Run the unit test**

Run: `nix develop --command go test ./internal/datapath -run TestTotalsSurviveApplies`
Expected: PASS.

- [ ] **Step 5: Fold before Apply**

shutdown.go:

```go
type HostDatapath struct {
	Handles datapath.Handles
	// Totals keeps counters across applies; nil skips the fold.
	Totals *datapath.Totals
	// CounterReader lists the live table; the real handles' *nftables.Conn.
	CounterReader datapath.CounterReader
}

func (d HostDatapath) Apply(ctx context.Context, state datapath.State) error {
	if d.Totals != nil && d.CounterReader != nil {
		if live, err := datapath.ReadCounters(d.CounterReader); err == nil {
			d.Totals.Fold(live)
		}
	}
	return datapath.Apply(ctx, datapath.Render(state), d.Handles)
}
```

A failed read skips the fold: counters for that pass restart at zero, which Prometheus reads as a reset, and the datapath still applies.

- [ ] **Step 6: Kernel test**

metrics_kernel_test.go, using the firewall test's harness (`inNetns`, `newNetns`, `vethInto`, `addrOn`, `defaultVia`, `udpEcho`, `udpExchange`, the fw* addresses):

```go
// TestCountsMappedTrafficOnRealKernel sends UDP through a mapping, applies the
// same plan again, sends more, and checks that kup-count counted both
// directions of both rounds and nothing from a flow DNAT'd elsewhere.
func TestCountsMappedTrafficOnRealKernel(t *testing.T) {
	inNetns(t, "TestCountsMappedTrafficOnRealKernel", countsMappedTraffic)
}
```

Body:
1. Build client, node and pod namespaces the way `deliversThroughHostFirewall` does (no host firewall).
2. State: one DNATRule `{Iface: "eth-client", Port: udp fwUDPPort, ToAddr: fwPod, Owner: "t/udp"}` plus the ExemptRule the firewall test uses, and a second DNATRule `{Iface: "eth-client", Port: udp 9999, ToAddr: fwPodGw, Owner: "t/other"}` for the unrelated flow.
3. `tot := &Totals{}`; `dp := agent-free fold`: call `live, _ := ReadCounters(conn); tot.Fold(live); Apply(...)` as HostDatapath does.
4. From the client, 5 `udpExchange` to `fwPub:fwUDPPort`.
5. Fold and Apply the same plan again.
6. 3 more exchanges.
7. `snap := tot.Snapshot(ReadCounters(conn))`.
8. Assert `snap[{ChainCount,"t/udp","eth-client","",IPv4,CtDirOriginal}].Packets == 8` and the reply key (Oif "eth-client", CtDirReply) `== 8`, and `snap[{ChainPre,"t/udp","eth-client",...}].Packets == 1` per Apply round in which a new flow started (each exchange uses a new source port, so 8).
9. Assert no key with Comment "t/other" has Packets > 0.

Run: `nix develop --command go test ./internal/datapath -run TestCountsMappedTrafficOnRealKernel -v`
Expected: PASS. Then break it on purpose: change `ct reply ip saddr` compare to use a 4-byte target in countExprs, rerun, see the reply count at 0 (proves the test catches a wrong padding), restore.

- [ ] **Step 7: Commit**

```bash
git add internal/datapath internal/agent/shutdown.go
git commit -m "feat(datapath): read rule counters and keep totals across applies"
```

---

### Task 4: Open flows from conntrack

**Files:**
- Create: `internal/datapath/conntrack.go`, `internal/datapath/conntrack_test.go`

**Interfaces:**
- Produces:

```go
type FlowKey struct{ Owner string; Deliver bool }
func CountFlows(flows []*netlink.ConntrackFlow, dnat []DNATRule) map[FlowKey]int
type ConntrackLister func() ([]*netlink.ConntrackFlow, error)
func DumpFlows(ctx context.Context, list ConntrackLister) ([]*netlink.ConntrackFlow, error) // 5 s cap
```

- [ ] **Step 1: Failing test**

```go
func TestCountFlows(t *testing.T) {
	pod := netip.MustParseAddr("10.244.18.186")
	linkEnd := netip.MustParseAddr("169.254.64.1")
	dnat := []DNATRule{
		{Iface: "enp1s0", Port: PortSel{Proto: "udp", First: 27015, Last: 27115}, ToAddr: pod, Owner: "g/srv"},
		{Iface: "kup-0", Port: PortSel{Proto: "udp", First: 27015, Last: 27115}, ToAddr: pod, DstAddr: &linkEnd, Owner: "g/srv"},
	}
	flow := func(dst string, dport uint16, replySrc string) *netlink.ConntrackFlow {
		return &netlink.ConntrackFlow{FamilyType: unix.AF_INET,
			Forward: netlink.IPTuple{Protocol: unix.IPPROTO_UDP, DstIP: net.ParseIP(dst), DstPort: dport},
			Reverse: netlink.IPTuple{Protocol: unix.IPPROTO_UDP, SrcIP: net.ParseIP(replySrc)}}
	}
	got := CountFlows([]*netlink.ConntrackFlow{
		flow("192.168.1.155", 27020, "10.244.18.186"), // accepted on enp1s0
		flow("169.254.64.1", 27100, "10.244.18.186"),  // arrived over kup-0
		flow("192.168.1.155", 28000, "10.244.18.186"), // port outside the range
		flow("10.244.18.186", 27020, "10.244.18.186"), // not translated
	}, dnat)
	want := map[FlowKey]int{{Owner: "g/srv"}: 1, {Owner: "g/srv", Deliver: true}: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CountFlows = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run, see it fail** (`undefined: CountFlows`).

- [ ] **Step 3: Implement**

```go
// CountFlows counts the conntrack entries each mapping's DNAT rules made: the
// protocol and original destination port inside the rule's selection, and
// the reply source equal to the rule's target. A reply source that differs
// from the original destination is what DNAT means; netlink's ConntrackFlow
// carries no status field. A flow whose original destination is a link's own
// end, a rule's DstAddr, counts as delivered over the link; every other
// matching flow as accepted.
func CountFlows(flows []*netlink.ConntrackFlow, dnat []DNATRule) map[FlowKey]int {
	out := map[FlowKey]int{}
	for _, f := range flows {
		orig, ok1 := netip.AddrFromSlice(f.Forward.DstIP)
		reply, ok2 := netip.AddrFromSlice(f.Reverse.SrcIP)
		if !ok1 || !ok2 || orig.Unmap() == reply.Unmap() {
			continue
		}
		orig, reply = orig.Unmap(), reply.Unmap()
		var hit *DNATRule
		deliver := false
		for i := range dnat {
			d := &dnat[i]
			if d.Owner == "" || protoNum(d.Port.Proto) != f.Forward.Protocol ||
				f.Forward.DstPort < d.Port.First || f.Forward.DstPort > d.Port.Last ||
				d.ToAddr.Unmap() != reply {
				continue
			}
			if d.DstAddr != nil {
				if d.DstAddr.Unmap() == orig {
					hit, deliver = d, true
					break
				}
				continue
			}
			if hit == nil {
				hit = d
			}
		}
		if hit != nil {
			out[FlowKey{Owner: hit.Owner, Deliver: deliver}]++
		}
	}
	return out
}

// DumpFlows lists the conntrack table in both families, giving up after 5
// seconds.
func DumpFlows(ctx context.Context, list ConntrackLister) ([]*netlink.ConntrackFlow, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	type res struct {
		f   []*netlink.ConntrackFlow
		err error
	}
	ch := make(chan res, 1)
	go func() { f, err := list(); ch <- res{f, err} }()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
```

The real lister, in the same file:

```go
// HostConntrack dumps both families' conntrack tables.
func HostConntrack() ([]*netlink.ConntrackFlow, error) {
	v4, err := netlink.ConntrackTableList(netlink.ConntrackTable, unix.AF_INET)
	if err != nil {
		return nil, err
	}
	v6, err := netlink.ConntrackTableList(netlink.ConntrackTable, unix.AF_INET6)
	if err != nil {
		return nil, err
	}
	return append(v4, v6...), nil
}
```

- [ ] **Step 4: Scale test**

```go
func BenchmarkCountFlows50k(b *testing.B) {
	// 50000 flows, 20 DNAT rules; reports ns/op for the spec's scrape budget.
}
```

Fill it with generated flows (half matching, half not). Run: `nix develop --command go test ./internal/datapath -run '^$' -bench CountFlows50k`. Record the ns/op in the commit message. Expected: well under 1 s.

- [ ] **Step 5: Run** `nix develop --command go test ./internal/datapath -run TestCountFlows`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/datapath/conntrack*.go
git commit -m "feat(datapath): count open flows per mapping from conntrack"
```

---

### Task 5: Mapping info rows from the reconcile

**Files:**
- Modify: `internal/reconcile/reconcile.go` (Result.Mappings)
- Test: `internal/reconcile/reconcile_test.go`

**Interfaces:**
- Produces:

```go
type MappingInfo struct {
	Namespace, Name, Class, Mode, Protocol, Port, EndpointNode string
}
// Result.Mappings []MappingInfo: one row per accepted mapping this node holds a DNAT rule for.
```

- [ ] **Step 1: Failing test**

In reconcile_test.go's single-mapping case:

```go
want := []MappingInfo{{Namespace: "udp-echo", Name: "udp-echo", Class: "public", Mode: "Single",
	Protocol: "UDP", Port: "33333", EndpointNode: "worker-b"}}
if !reflect.DeepEqual(res.Mappings, want) {
	t.Errorf("Mappings = %+v, want %+v", res.Mappings, want)
}
```

Use the fixture's real names and port. A second assertion: a node that holds no rule for the mapping gets `res.Mappings == nil`. A range mapping renders Port as `"27015-27115"`.

- [ ] **Step 2: Run, see it fail.**

- [ ] **Step 3: Implement** — after the state is built in Compute, collect owners present in `st.DNAT` into a set, then for each accepted mapping whose `ownerOfPortMap(m.pm)` is in the set append a row: Class `m.class.Name`, Mode `string(servingModeOf(m.class))`, Protocol `string(m.pm.Spec.Protocol)`, Port from the spec (single or `first-last`), EndpointNode `m.endpoint.node` when the endpoint is set. Sort by namespace, name.

- [ ] **Step 4: Run** `nix develop --command go test ./internal/reconcile`. Expected: PASS.

- [ ] **Step 5: Commit** `git commit -am "feat(reconcile): report the mappings this node programs"`

---

### Task 6: The collector

**Files:**
- Create: `internal/metrics/collector.go`, `internal/metrics/collector_test.go`
- Modify: `internal/agent/agent.go` (publish a snapshot after each pass; apply counters)

**Interfaces:**
- Consumes: `datapath.CounterKey/Value`, `datapath.Totals`, `datapath.CountFlows`, `datapath.DumpFlows`, `reconcile.MappingInfo`, `v1alpha1.Link`.
- Produces:

```go
type Snapshot struct {
	Mappings []reconcile.MappingInfo
	DNAT     []datapath.DNATRule
	Links    []LinkInfo // device, slot, class, peer
}
type LinkInfo struct{ Device, Slot, Class, Peer string }
type Source interface {
	Snapshot() Snapshot
	Counters() (map[datapath.CounterKey]datapath.CounterValue, error) // totals + live
	Flows(ctx context.Context) ([]*netlink.ConntrackFlow, error)
}
func New(src Source) *Collector // implements prometheus.Collector
// Agent-side counters, registered by the agent:
var ApplyTotal *prometheus.CounterVec          // kuport_apply_total{result}
var LastApplySuccess prometheus.Gauge           // kuport_last_apply_success_timestamp_seconds
var StateEntries *prometheus.GaugeVec           // kuport_state_entries{kind}
var ConntrackDumpErrors prometheus.Counter      // kuport_conntrack_dump_errors_total
```

- [ ] **Step 1: Failing test with testutil**

```go
func TestCollectorSeries(t *testing.T) {
	src := fakeSource{
		snap: Snapshot{
			Mappings: []reconcile.MappingInfo{{Namespace: "jellyfin", Name: "jellyfin", Class: "quasar-w-1", Mode: "Single", Protocol: "TCP", Port: "10002", EndpointNode: "worker-1"}},
			DNAT: []datapath.DNATRule{{Iface: "enp1s0", Port: datapath.PortSel{Proto: "tcp", First: 10002, Last: 10002}, ToAddr: netip.MustParseAddr("10.244.18.186"), Owner: "jellyfin/jellyfin"}},
			Links: []LinkInfo{{Device: "kup-0", Slot: "0", Class: "netcup-bar-w-1", Peer: "worker-2"}},
		},
		counters: map[datapath.CounterKey]datapath.CounterValue{
			{Chain: datapath.ChainCount, Comment: "jellyfin/jellyfin", Iif: "enp1s0", Dir: datapath.CtDirOriginal}: {Packets: 10, Bytes: 1000},
			{Chain: datapath.ChainCount, Comment: "jellyfin/jellyfin", Oif: "enp1s0", Dir: datapath.CtDirReply}:    {Packets: 9, Bytes: 9000},
			{Chain: datapath.ChainPre, Comment: "jellyfin/jellyfin", Iif: "enp1s0"}:                                 {Packets: 2},
			{Chain: datapath.ChainMSS}:                                                                               {Packets: 4},
			{Chain: datapath.ChainEgress, Comment: "egress:qb/qb/qb-0", Family: datapath.FamilyIPv4}:               {Packets: 7, Bytes: 700},
		},
	}
	want := `
# HELP kuport_mapping_packets_total Packets of a mapping's flows leaving this node, per direction.
# TYPE kuport_mapping_packets_total counter
kuport_mapping_packets_total{direction="reply",family="ipv4",interface="enp1s0",namespace="jellyfin",portmap="jellyfin",role="accept"} 9
kuport_mapping_packets_total{direction="request",family="ipv4",interface="enp1s0",namespace="jellyfin",portmap="jellyfin",role="accept"} 10
`
	if err := testutil.CollectAndCompare(New(src), strings.NewReader(want), "kuport_mapping_packets_total"); err != nil {
		t.Fatal(err)
	}
}
```

Add one CollectAndCompare per metric family in the same test: bytes, connections (`kuport_mapping_connections_total ... 2`), egress (`kuport_egress_packets_total{egress="qb",family="ipv4",namespace="qb",path="link",pod="qb-0"} 7`), mss (`kuport_mss_clamped_total 4`), link info, mapping info. A second test feeds a counter key with `Iif: "kup-0"` and expects `role="deliver"`, and an egress key from a rule whose comment belongs to a blocked pod. The blocked path comes from the snapshot: add `Egress []EgressInfo{Owner, Path}` to Snapshot when writing this test, filled from `res.EgressStatus` rows for pods on this node.

A third test: `Flows` returns an error and the scrape omits `kuport_mapping_open_flows` and increments `kuport_conntrack_dump_errors_total`.

- [ ] **Step 2: Run, see it fail.**

- [ ] **Step 3: Implement**

Descriptors as `prometheus.NewDesc` with the label sets from the spec. `Collect`:

1. `snap := src.Snapshot()`; emit `kuport_mapping_info` (value 1) per row; `kuport_link_info` per link.
2. `c, err := src.Counters()`; on error skip traffic series. For each key:
   - ChainCount: split Comment into namespace/portmap at the first `/`; interface = Iif or Oif; role = `deliver` if the interface starts with `kup-`, else `accept`; direction from Dir; family `ipv4`/`ipv6`. Emit packets and bytes as `prometheus.MustNewConstMetric(..., prometheus.CounterValue, ...)`.
   - ChainPre: `kuport_mapping_connections_total` with interface Iif, role from the interface, family.
   - ChainEgress: Comment `egress:<ns>/<egress>/<pod>` → labels; path from snap.Egress by Owner (`link` default, `blocked` when the snapshot says so).
   - ChainMSS: `kuport_mss_clamped_total`.
   Sum keys that map to the same label set before emitting (two IPv4 count rules on one interface for one mapping with two targets, which a dual-stack link end does not produce but a pod with two IPv4 addresses could).
3. `flows, err := src.Flows(ctx with 5 s)`; on error `ConntrackDumpErrors.Inc()` and skip; else `datapath.CountFlows(flows, snap.DNAT)` → `kuport_mapping_open_flows{namespace,portmap,role}`.

Register `ApplyTotal`, `LastApplySuccess`, `StateEntries`, `ConntrackDumpErrors` on `metrics.Registry` (controller-runtime) in an `init()`.

- [ ] **Step 4: Agent publishes the snapshot**

agent.go: the Reconciler gains

```go
	// snap is what the metrics collector reads: the last pass's mappings, DNAT
	// rules, links and egress paths on this node.
	snap atomic.Pointer[metrics.Snapshot]
	// Totals and CounterReader are the same values HostDatapath holds, shared
	// with the collector.
	Totals        *datapath.Totals
	CounterReader datapath.CounterReader
```

and implements `metrics.Source`:

```go
func (r *Reconciler) Snapshot() metrics.Snapshot {
	if s := r.snap.Load(); s != nil {
		return *s
	}
	return metrics.Snapshot{}
}

func (r *Reconciler) Counters() (map[datapath.CounterKey]datapath.CounterValue, error) {
	live, err := datapath.ReadCounters(r.CounterReader)
	if err != nil {
		return nil, err
	}
	return r.Totals.Snapshot(live), nil
}

func (r *Reconciler) Flows(ctx context.Context) ([]*netlink.ConntrackFlow, error) {
	return datapath.DumpFlows(ctx, datapath.HostConntrack)
}
```


In Reconcile, after applyState succeeds, store the snapshot built from `res.Mappings`, `res.State.DNAT`, the Links (from `in.Links` where `r.NodeName` is in `Spec.Peers`, device `kup-<slot>` parsed from the Link's name `slot-<n>`, class `Spec.ClassName`, peer the other node), and egress rows.

In applyState:

```go
	if err := r.DP.Apply(ctx, st); err != nil {
		metrics.ApplyTotal.WithLabelValues("error").Inc()
		return err
	}
	metrics.ApplyTotal.WithLabelValues("ok").Inc()
	metrics.LastApplySuccess.SetToCurrentTime()
	for kind, n := range map[string]int{"dnat": len(st.DNAT), "exempt": len(st.Exempt), "mark": len(st.Mark),
		"link": len(st.Links), "rule": len(st.Rules), "route": len(st.Routes)} {
		metrics.StateEntries.WithLabelValues(kind).Set(float64(n))
	}
```

Agent test: with the fake Datapath, one reconcile; assert `testutil.ToFloat64(metrics.ApplyTotal.WithLabelValues("ok")) == 1` and the snapshot holds the fixture mapping.

- [ ] **Step 5: Run** `nix develop --command go test ./internal/metrics ./internal/agent`. Expected: PASS.

- [ ] **Step 6: Commit** `git add internal/metrics internal/agent && git commit -m "feat(metrics): collector for mapping, egress, link and apply series"`

---

### Task 7: Endpoint, flags and manifests

**Files:**
- Create: `internal/metrics/listen.go`, `internal/metrics/listen_test.go`
- Modify: `cmd/kuport-agent/main.go`, `cmd/kuport-agent/main_test.go` (if present; else create)
- Modify: `deploy/daemonset.yaml`, `chart/templates/daemonset.yaml`, `chart/values.yaml`

**Interfaces:**
- Produces:

```go
type BindConfig struct {
	Bind   string // "" = HostIP, "off", or an interface name
	HostIP string
	Port   uint16
}
type AddrLookup func(iface string) ([]netip.Addr, error) // global unicast addresses
func ResolveAddrs(c BindConfig, lookup AddrLookup) ([]netip.AddrPort, error)
type Server struct{ Config BindConfig; Lookup AddrLookup; Handler http.Handler; Log logr.Logger; Retry time.Duration }
func (s *Server) Start(ctx context.Context) error // manager.Runnable
```

- [ ] **Step 1: Failing tests**

```go
func TestResolveAddrs(t *testing.T) {
	lookup := func(name string) ([]netip.Addr, error) {
		if name == "wt0" {
			return []netip.Addr{netip.MustParseAddr("100.64.234.169"), netip.MustParseAddr("fd00::5")}, nil
		}
		return nil, fmt.Errorf("no interface %s", name)
	}
	cases := []struct {
		c    BindConfig
		want []string
		err  bool
	}{
		{BindConfig{HostIP: "100.64.234.169", Port: 9790}, []string{"100.64.234.169:9790"}, false},
		{BindConfig{Bind: "wt0", Port: 9790}, []string{"100.64.234.169:9790", "[fd00::5]:9790"}, false},
		{BindConfig{Bind: "off", Port: 9790}, nil, false},
		{BindConfig{HostIP: "", Port: 9790}, nil, true},
		{BindConfig{Bind: "eth9", Port: 9790}, nil, true},
		{BindConfig{HostIP: "0.0.0.0", Port: 9790}, nil, true}, // never a wildcard
	}
	for _, tc := range cases { /* compare */ }
}

func TestServerRetriesUntilAddressAppears(t *testing.T) {
	// Lookup fails twice, then returns 127.0.0.1; Retry 10ms; Port 0 is not
	// allowed, so pick a free port with net.Listen first and close it.
	// Expect GET http://127.0.0.1:<port>/metrics to answer 200 within 1s,
	// and Start to return nil when ctx is cancelled.
}
```

- [ ] **Step 2: Run, see them fail.**

- [ ] **Step 3: Implement listen.go**

ResolveAddrs: `off` → nil, nil. Empty Bind → parse HostIP; invalid or unspecified (`IsUnspecified`) → error. Interface → `lookup`; keep `IsGlobalUnicast()` addresses only; none → error. Never return an unspecified address.

Server.Start: loop until ctx done: resolve; on error log once per streak ("metrics endpoint not bound; retrying") and wait Retry (default 30 s); on success open one `net.Listen("tcp", ap.String())` per address, serve `s.Handler` on each with `http.Server{ReadHeaderTimeout: 5 * time.Second}`, log "serving metrics" with the addresses, then block on ctx and shut the servers down. A Listen failure closes the opened listeners and retries.

`HostAddrs` lookup for production: `net.InterfaceByName` + `Addrs()`, converted to netip.

- [ ] **Step 4: Flags**

main.go config gains `metricsBind string`, `metricsPort uint16`, `hostIP string`:

```go
	metricsBind := fs.String("metrics-bind", getenv("METRICS_BIND"),
		"where /metrics listens: empty for the node's InternalIP (HOST_IP), an interface name such as wt0, or off")
	metricsPort := fs.Uint("metrics-port", uintOr(getenv, "METRICS_PORT", 9790), "TCP port of /metrics")
```

`hostIP: getenv("HOST_IP")`. In run, after the reconciler:

```go
	reg := ctrlmetrics.Registry
	reg.MustRegister(metrics.New(r))
	if err := mgr.Add(&metrics.Server{
		Config:  metrics.BindConfig{Bind: cfg.metricsBind, HostIP: cfg.hostIP, Port: cfg.metricsPort},
		Lookup:  metrics.HostAddrs,
		Handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		Log:     log.WithName("metrics"),
	}); err != nil {
		return fmt.Errorf("add metrics server: %w", err)
	}
```

Leave `Metrics: metricsserver.Options{BindAddress: "0"}` as it is. Wire `dp := agent.HostDatapath{Handles: handles, Totals: totals, CounterReader: nftConn}` where `totals := &datapath.Totals{}` and nftConn is `handles.NFT.(*nftables.Conn)` (add `datapath.Handles.Counters()` returning the conn as a CounterReader so main does no type assertion), and pass the same Totals and reader to the Reconciler.

parseFlags test: `METRICS_BIND=wt0 METRICS_PORT=9800 HOST_IP=100.64.0.1` resolve to the config fields; defaults are `""`, 9790.

- [ ] **Step 5: Manifests**

deploy/daemonset.yaml env, after NODE_NAME:

```yaml
        # The node's InternalIP. /metrics listens there and nowhere else
        # unless METRICS_BIND names an interface.
        - name: HOST_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
```

and under the container:

```yaml
        ports:
        - name: metrics
          containerPort: 9790
          protocol: TCP
```

chart/values.yaml:

```yaml
# Where /metrics listens. Empty binds the node's InternalIP only; an
# interface name such as wt0 binds that interface's addresses; off turns the
# endpoint off. The agent never binds a wildcard address.
metrics:
  bind: ""
  port: 9790

# UDP port of every return link. Must differ from the CNI's tunnel port, 8472
# for Cilium.
linkPort: 4790
```

chart/templates/daemonset.yaml: HOST_IP as above; `METRICS_BIND` `{{ .Values.metrics.bind | quote }}`, `METRICS_PORT` `{{ .Values.metrics.port | quote }}`, `LINK_PORT` `{{ .Values.linkPort | quote }}`; the `metrics` container port from `.Values.metrics.port`.

Run: `nix develop --command helm lint chart && nix develop --command helm template chart | grep -A3 METRICS_BIND`. Expected: lint passes; the env renders.

- [ ] **Step 6: Run** `nix develop --command go test ./...`. Expected: PASS.

- [ ] **Step 7: Commit** `git add -A cmd internal/metrics deploy chart && git commit -m "feat(agent): serve /metrics on the node's InternalIP or a named interface"`

---

### Task 8: Docs and release

**Files:**
- Create: `docs/metrics.md`
- Modify: `docs/api.md` (Agent flags), `docs/datapath.md` (kup-count, comments), `README.md` (one line linking docs/metrics.md)
- Modify: `.cortex/plans/2026-10-13-metrics/design.md` ("Changes made while building" table, as the egress spec has)

- [ ] **Step 1:** Load the catalyst-v2-writing-docs skill. Write docs/metrics.md: the bind rules, every series with labels and source, `role` and `direction` with the two-node example, the counters-across-applies behaviour, and sample output pasted from a kernel-test scrape (run the collector in TestCountsMappedTrafficOnRealKernel and log `testutil.CollectAndFormat` output; paste that, not hand-written text).
- [ ] **Step 2:** docs/datapath.md: add kup-count to "Rules on an accepting node" and "Rules on the pod's node" listings, and a paragraph on comments. docs/api.md: the three flags and HOST_IP.
- [ ] **Step 3:** Run the skill's final grep over each doc; fix hits.
- [ ] **Step 4:** Commit `docs: metrics`, push master, wait for CI green: `gh run list -R walzen-group/kuport --branch master --limit 1`.
- [ ] **Step 5:** Tag and push `v0.9.0`; wait for the release workflow; check the asset: `curl -fsSL https://github.com/walzen-group/kuport/releases/download/v0.9.0/kuport-0.9.0.yaml | grep -n 'HOST_IP\|containerPort: 9790'`. Expected: both present.

---

### Task 9: Infra changes (each needs the admin)

Work in /home/nixos/repos/infra. Edit files, plan, show the admin; the admin applies the talos and kube-prometheus units and gives the permission sentence for each commit.

**Files:**
- Modify: `modules/infra/talos/opentofu/patches/firewall.tf`
- Modify: `environments/prod/networking/kuport/inputs.yaml` (kuport_version)
- Modify: `modules/cluster/kube-prometheus/opentofu/manifests.tf`, `main.tf` (chart values), `README.md`
- Create: `flux/templates/management/grafana/base/dashboards/kuport.json`; modify the grafana base kustomization's dashboard list
- Modify: `modules/networking/kuport/opentofu/README.md`

- [ ] **Step 1: Firewall rule**

In `local.firewall_rules`, after `kuport-return`:

```hcl
      { name = "kuport-metrics", ports = [local.kuport_metrics_port], protocol = "tcp", sources = concat(var.cluster.overlay_subnets, local.pod_subnets) },
```

and `kuport_metrics_port = 9790` next to `kuport_return_port`, under the same comment, extended to name the metrics port. Plan the prod talos unit (read-only) and show the admin the planned NetworkRuleConfig additions. The admin applies.

- [ ] **Step 2: kuport bump**

`kuport_version: "v0.9.0"`; plan; show; apply only if the admin approved updating the cluster in this session, otherwise hand the apply to the admin.

- [ ] **Step 3: PodMonitor and kube-state-metrics**

manifests.tf, following `kubectl_manifest.backup_controller_pod_monitor`:

```hcl
resource "kubectl_manifest" "kuport_pod_monitor" {
  depends_on = [helm_release.kube_prometheus_stack]
  yaml_body = yamlencode({
    apiVersion = "monitoring.coreos.com/v1"
    kind       = "PodMonitor"
    metadata   = { name = "kuport", namespace = var.namespace }
    spec = {
      namespaceSelector   = { any = true }
      selector            = { matchLabels = { "app.kubernetes.io/name" = "kuport" } }
      podMetricsEndpoints = [{ port = "metrics", honorLabels = true, relabelings = [{ sourceLabels = ["__meta_kubernetes_pod_node_name"], targetLabel = "node" }] }]
    }
  })
}
```

Copy depends_on and namespace references from the backup-controller resource exactly; the names above are what to look for.

kube-state-metrics in the chart values (main.tf), under `kube-state-metrics`:

```hcl
rbac = { extraRules = [{ apiGroups = ["kuport.wlz.li"], resources = ["portmaps", "egresses", "links"], verbs = ["list", "watch"] }] }
customResourceState = {
  enabled = true
  config = {
    spec = {
      resources = [
        {
          groupVersionKind = { group = "kuport.wlz.li", version = "v1alpha1", kind = "PortMap" }
          metricNamePrefix = "kube_customresource_kuport_portmap"
          metrics = [{
            name = "condition"
            each = { type = "Gauge", gauge = { path = ["status", "conditions"], labelsFromPath = { type = ["type"], reason = ["reason"] }, valueFrom = ["status"] } }
          }]
        },
        {
          groupVersionKind = { group = "kuport.wlz.li", version = "v1alpha1", kind = "Egress" }
          metricNamePrefix = "kube_customresource_kuport_egress"
          metrics = [{
            name = "pod_path"
            each = { type = "Info", info = { path = ["status", "pods"], labelsFromPath = { pod = ["name"], path = ["path"], node = ["node"] } } }
          }]
        },
        {
          groupVersionKind = { group = "kuport.wlz.li", version = "v1alpha1", kind = "Link" }
          metricNamePrefix = "kube_customresource_kuport_link"
          metrics = [{
            name = "info"
            each = { type = "Info", info = { labelsFromPath = { class = ["spec", "className"] } } }
          }]
        },
      ]
    }
  }
}
```

Before writing it, look up the kube-state-metrics CustomResourceState schema for the chart version the unit pins (Context7 or the chart's values.yaml) and match key names exactly; a condition's `status` is the string "True"/"False", which needs `valueFrom` mapping (`nilIsZero`, or `stateSet` with list ["True","False","Unknown"]). Pick the form the docs give for conditions.

kube-prometheus needs an edge to the kuport unit's CRDs? No: kube-state-metrics only lists the CRs at runtime and logs while they are absent; the PodMonitor needs only the prometheus-operator CRDs the unit already orders after. State this in the README section.

Plan the kube-prometheus unit; show; the admin applies. Verify after:

```sh
kubectl -n monitoring get podmonitor kuport
```

and in Prometheus: `up{job="monitoring/kuport"}` returns six series at 1.

- [ ] **Step 4: Dashboard**

Write kuport.json (Grafana 13 schema, `"timezone": "browser"`, uid `kuport`, title `kuport`). Variables: `class` (`label_values(kuport_mapping_info, class)`), `namespace`, `portmap`, `node` (multi, include All). Rows and queries:

| Panel | Query |
| --- | --- |
| Agents up (stat) | `sum(up{job="monitoring/kuport"})` |
| Oldest apply (stat, seconds) | `time() - min(kuport_last_apply_success_timestamp_seconds)` |
| Apply errors 1h (stat) | `sum(increase(kuport_apply_total{result="error"}[1h]))` |
| PortMaps not Programmed (stat) | `count(kube_customresource_kuport_portmap_condition{type="Programmed"} == 0) or vector(0)` (adjust to the CRS form chosen in Step 3) |
| Egress pods Blocked (stat) | `count(kube_customresource_kuport_egress_pod_path_info{path="Blocked"}) or vector(0)` |
| Links in use (gauge, max 1024) | `count(kube_customresource_kuport_link_info) or vector(0)` |
| Mapping graph (node graph) | edges: `sum by (namespace, portmap, interface, node) (rate(kuport_mapping_bytes_total{role="accept"}[5m]))` with `label_join`/`label_replace` building `source` = `node/interface`, `target` = `namespace/portmap`, `id`; plus `portmap → endpoint_node` from kuport_mapping_info and `portmap → kup-n` from the deliver series |
| Mappings (table) | join on namespace,portmap: info; `sum by (namespace,portmap) (rate(kuport_mapping_packets_total{role="accept",direction="request"}[5m]))`; same for reply; `sum by (namespace,portmap) (rate(kuport_mapping_connections_total{role="accept"}[5m]))`; `sum by (namespace,portmap) (kuport_mapping_open_flows{role="accept"})`. Override: replies/s cell red when requests/s > 0 and replies/s == 0 |
| Bytes/s per mapping | `sum by (namespace, portmap, direction) (rate(kuport_mapping_bytes_total{role="accept"}[$__rate_interval]))` |
| Connections/s per mapping | `sum by (namespace, portmap) (rate(kuport_mapping_connections_total{role="accept"}[$__rate_interval]))` |
| Requests per accepting node (Multi) | `sum by (node) (rate(kuport_mapping_packets_total{role="accept",direction="request"}[$__rate_interval]) * on (namespace, portmap) group_left kuport_mapping_info{mode="Multi"})` |
| Return links (table) | `kuport_link_info * on (node, device) group_right ... rate(node_network_transmit_bytes_total{device=~"kup-.*"}[5m])`, receive, `increase(node_network_transmit_errs_total{device=~"kup-.*"}[1h])`. node-exporter's `instance` must be mapped to `node`: join through `node_uname_info` or relabel; check which label the chart's node-exporter series carry first |
| Link bytes/s (time series) | `rate(node_network_transmit_bytes_total{device=~"kup-.*"}[$__rate_interval])` and receive |
| Egress bytes/s | `sum by (namespace, egress, pod) (rate(kuport_egress_bytes_total{path="link"}[$__rate_interval]))` |
| Egress blocked packets | `sum by (namespace, egress, pod) (rate(kuport_egress_packets_total{path="blocked"}[$__rate_interval]))` |
| SYNs clamped | `sum by (node) (rate(kuport_mss_clamped_total[$__rate_interval]))` |
| Applies per node | `sum by (node, result) (increase(kuport_apply_total[$__rate_interval]))` |
| Reconcile duration | `histogram_quantile(0.95, sum by (le, node) (rate(controller_runtime_reconcile_time_seconds_bucket{controller="kuport-agent"}[$__rate_interval])))` |
| State entries | `kuport_state_entries` by node, kind |

Add the file to the grafana base kustomization's configMapGenerator the way backups.json is listed. Check the node graph renders in Grafana before committing; if the merged edge queries do not produce `id`/`source`/`target` frames, switch to a `kuport_edge` series (a follow-up kuport change) and say so to the admin.

- [ ] **Step 5: README**

modules/networking/kuport/opentofu/README.md: a "Metrics" section: port 9790 on the InternalIP, the firewall rule, the PodMonitor in kube-prometheus, the board's path. Load catalyst-v2-writing-docs first; run its grep and the `${`/`<` checks.

- [ ] **Step 6: Hand over**

List the edited files and the commit message for each infra change; wait for the permission sentence before committing.

---

## Self-review

- Spec coverage: endpoint (T7), comments (T1), counters across applies (T3), kup-count (T2), metrics table (T5, T6), role (T6), open flows (T4, T6), infra four changes (T9), dashboard (T9), tests (T1-T7), docs (T8).
- Type names checked across tasks: `CounterKey`, `CounterValue`, `Totals`, `CounterReader`, `ReadCounters`, `CountFlows`, `DumpFlows`, `HostConntrack`, `MappingInfo`, `Snapshot`, `LinkInfo`, `Source`, `BindConfig`, `ResolveAddrs`, `Server`. The field is `CounterReader` in HostDatapath (Task 3), the Reconciler (Task 6) and main (Task 7); the Reconciler's method is `Counters()`.
