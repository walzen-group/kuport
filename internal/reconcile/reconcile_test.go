package reconcile

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// claim is a shorthand for an existing LinkAllocation in a class's status.
func claim(a, b string, slot int32) v1alpha1.LinkAllocation {
	p := sortedPair(a, b)
	return v1alpha1.LinkAllocation{
		Key:    pairKey(a, b),
		Peers:  []string{p[0], p[1]},
		Subnet: "169.254.77.0/31",
		Slot:   slot,
	}
}

// remoteWorld is node-a (accepting) plus a pod on node-b, with an existing
// return-link claim so both ends can build their link at a known slot.
func remoteWorld(runOn string, opts ...classOpt) Inputs {
	allOpts := append([]classOpt{withLinks(claim("node-a", "node-b", 0))}, opts...)
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", nil),
			node("node-c", "10.0.0.3", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes:    []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"}, allOpts...)},
		PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-b", target: "pod-a"}),
		},
		Now: metav1.NewTime(baseTime),
	}
}

func TestRoles(t *testing.T) {
	t.Run("accepting only", func(t *testing.T) {
		st := Compute(remoteWorld("node-a")).State
		assertCounts(t, st, counts{dnat: 1, exempt: 1, mark: 0, links: 1, rules: 0, routes: 0})
		if st.Exempt[0].OifName != "cilium_host" || st.Exempt[0].Negate {
			t.Errorf("exempt = %+v, want cilium_host non-negated", st.Exempt[0])
		}
	})

	t.Run("target only", func(t *testing.T) {
		st := Compute(remoteWorld("node-b")).State
		assertCounts(t, st, counts{dnat: 0, exempt: 1, mark: 1, links: 1, rules: 2, routes: 1})
		if st.Exempt[0].OifName != "cilium_*" || !st.Exempt[0].Negate || !st.Exempt[0].PortIsSrc {
			t.Errorf("exempt = %+v, want cilium_* negated PortIsSrc", st.Exempt[0])
		}
		if st.Mark[0].Mark != markBase {
			t.Errorf("mark = %#x, want %#x", st.Mark[0].Mark, markBase)
		}
	})

	t.Run("both on one node (co-located)", func(t *testing.T) {
		// Single accepting node that also hosts the pod: DNAT only.
		in := Inputs{
			NodeName:   "node-a",
			Nodes:      []*corev1.Node{node("node-a", "10.0.0.1", map[string]string{"edge": "true"})},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes:    []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"})},
			PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.0.9", node: "node-a", target: "pod-a"}),
			},
			Now: metav1.NewTime(baseTime),
		}
		assertCounts(t, Compute(in).State, counts{dnat: 1, exempt: 1, mark: 0, links: 0, rules: 0, routes: 0})
	})

	t.Run("neither", func(t *testing.T) {
		assertCounts(t, Compute(remoteWorld("node-c")).State, counts{})
	})
}

func TestRefusals(t *testing.T) {
	t.Run("mode None with a remote pod", func(t *testing.T) {
		// node-a accepting, pod on node-b, return path None: refused everywhere.
		accepting := Compute(remoteWorld("node-a", withMode(v1alpha1.ReturnPathNone)))
		if len(accepting.State.DNAT) != 0 {
			t.Errorf("node-a DNAT = %d, want 0 (refused)", len(accepting.State.DNAT))
		}
		// The serving node (node-a) owns the status and reports the reason;
		// the endpoint holder reads it, it does not write it.
		holder := Compute(remoteWorld("node-b", withMode(v1alpha1.ReturnPathNone)))
		if hasStatus(holder, "games", "a") {
			t.Error("node-b holds the endpoint but is not the serving node")
		}
		c := programmedOf(accepting, "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonReturnPathUnavailable {
			t.Fatalf("Programmed = %s/%s, want False/ReturnPathUnavailable", c.Status, c.Reason)
		}
	})

	t.Run("two accepting nodes with a remote pod", func(t *testing.T) {
		build := func(runOn string) Inputs {
			return Inputs{
				NodeName: runOn,
				Nodes: []*corev1.Node{
					node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
					node("node-b", "10.0.0.2", map[string]string{"edge": "true"}),
					node("node-c", "10.0.0.3", nil),
				},
				Namespaces: []*corev1.Namespace{ns("games", nil)},
				Classes: []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"},
					withLinks(claim("node-a", "node-c", 0)))},
				PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
				Slices: []*discoveryv1.EndpointSlice{
					slice("games", "a", endpointSpec{addr: "10.244.9.9", node: "node-c", target: "pod-a"}),
				},
				Now: metav1.NewTime(baseTime),
			}
		}
		// The deterministic choice picks no candidate on an accepting node
		// (there is none), so the pod's node is not S: node-a, the first
		// accepting node, serves across a return link and the second
		// accepting node stays out.
		if got := len(Compute(build("node-a")).State.DNAT); got != 1 {
			t.Errorf("node-a DNAT = %d, want 1 (the serving node programs)", got)
		}
		if got := len(Compute(build("node-b")).State.DNAT); got != 0 {
			t.Errorf("node-b DNAT = %d, want 0 (the other accepting node is refused)", got)
		}
		// The serving node owns the status and reports the limitation; the
		// endpoint holder contributes only its half of the return path.
		c := programmedOf(Compute(build("node-a")), "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonRemotePodMultipleAcceptingNodes {
			t.Fatalf("Programmed = %s/%s, want False/RemotePodMultipleAcceptingNodes", c.Status, c.Reason)
		}
	})
}

// TestDeterminismUnderShuffle shuffles every input slice with a seeded RNG,
// recomputes 20 times, and asserts each Result deep-equals the first. Sorting
// bugs are the whole risk in this package and a single run will not find them.
func TestDeterminismUnderShuffle(t *testing.T) {
	base := richWorld()
	want := Compute(cloneInputs(base))

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		got := Compute(shuffleInputs(cloneInputs(base), rng))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle iteration %d produced a different Result", i)
		}
	}
}

// TestAgentAgreement runs one fixture from three nodes' points of view and
// asserts they agree on the chosen endpoint, the accepting node, and the link
// slot. Two agents disagreeing takes the port down with every rule looking
// correct, so this is the load-bearing check.
func TestAgentAgreement(t *testing.T) {
	world := func(runOn string) Inputs {
		return Inputs{
			NodeName: runOn,
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
				node("node-b", "10.0.0.2", nil),
				node("node-c", "10.0.0.3", nil),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes: []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"},
				withLinks(claim("node-a", "node-b", 5)))},
			PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-b", target: "pod-a"}),
			},
			Now: metav1.NewTime(baseTime),
		}
	}

	a := Compute(world("node-a")) // accepting node
	b := Compute(world("node-b")) // target node, holds the pod
	c := Compute(world("node-c")) // bystander

	// Chosen endpoint: node-a forwards to it, node-b marks replies from it.
	if len(a.State.DNAT) != 1 || len(b.State.Mark) != 1 {
		t.Fatalf("expected node-a DNAT and node-b Mark, got %d/%d", len(a.State.DNAT), len(b.State.Mark))
	}
	if a.State.DNAT[0].ToAddr.String() != b.State.Mark[0].SrcAddr.String() {
		t.Errorf("endpoint disagreement: node-a DNAT %s vs node-b mark %s",
			a.State.DNAT[0].ToAddr, b.State.Mark[0].SrcAddr)
	}
	if a.State.DNAT[0].ToAddr.String() != "10.244.5.5" {
		t.Errorf("chosen endpoint = %s, want 10.244.5.5", a.State.DNAT[0].ToAddr)
	}

	// Accepting node: node-b's link points its outer header at node-a, and its
	// return route goes to node-a's end of the /31.
	if len(a.State.Links) != 1 || len(b.State.Links) != 1 {
		t.Fatalf("expected a link on both node-a and node-b")
	}
	if b.State.Links[0].RemoteAddr.String() != "10.0.0.1" {
		t.Errorf("node-b link RemoteAddr = %s, want node-a 10.0.0.1", b.State.Links[0].RemoteAddr)
	}

	// Slot: both ends address the same /31, so both derived the same slot.
	if a.State.Links[0].LinkAddr.Masked() != b.State.Links[0].LinkAddr.Masked() {
		t.Errorf("slot disagreement: node-a /31 %s vs node-b /31 %s",
			a.State.Links[0].LinkAddr.Masked(), b.State.Links[0].LinkAddr.Masked())
	}
	if b.State.Mark[0].Mark != markBase|5 {
		t.Errorf("node-b mark = %#x, want slot 5", b.State.Mark[0].Mark)
	}

	// The bystander programs nothing for this mapping and owns no status.
	assertCounts(t, c.State, counts{})
	if !hasStatus(a, "games", "a") {
		t.Errorf("serving node node-a should own the PortMap status")
	}
	if hasStatus(b, "games", "a") {
		t.Errorf("endpoint holder node-b must not own the PortMap status")
	}
}

// multiAcceptingWorld is the F1 shape: a Service with ready endpoints on two
// accepting nodes (node-a, node-b) and a claim between them. The deterministic
// rule puts the chosen endpoint on the lexicographically first accepting node
// (node-a, whose pod is named higher), so node-a serves locally and no return
// link is needed.
func multiAcceptingWorld(runOn string) Inputs {
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", map[string]string{"edge": "true"}),
			node("node-c", "10.0.0.3", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes: []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"},
			withLinks(claim("node-a", "node-b", 0)))},
		PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a",
				endpointSpec{addr: "10.244.7.7", node: "node-a", target: "pod-zeta"},
				endpointSpec{addr: "10.244.8.8", node: "node-b", target: "pod-alpha"},
			),
		},
		Now: metav1.NewTime(baseTime),
	}
}

// TestAgentAgreementMultiAccepting extends the TestAgentAgreement shape to the
// F1 fixture: with ready endpoints on two accepting nodes every viewpoint must
// pick the same endpoint (the one on node-a, the first accepting node, even
// though node-b's pod is named lower), exactly one node programs, the other
// accepting node contributes no rules and computes the refusal, and only the
// serving node owns the PortMap status.
func TestAgentAgreementMultiAccepting(t *testing.T) {
	a := Compute(multiAcceptingWorld("node-a"))
	b := Compute(multiAcceptingWorld("node-b"))
	c := Compute(multiAcceptingWorld("node-c"))

	// Exactly one node programs: node-a DNATs to its local pod.
	if len(a.State.DNAT) != 1 || a.State.DNAT[0].ToAddr.String() != "10.244.7.7" {
		t.Errorf("node-a DNAT = %+v, want one rule to 10.244.7.7", a.State.DNAT)
	}

	// The other accepting node holds a ready pod and still programs nothing:
	// not the serving node, not the chosen pod's node.
	assertCounts(t, b.State, counts{})

	// The bystander contributes nothing.
	assertCounts(t, c.State, counts{})

	// The refusal is the shared computation: node-a, the serving node, owns
	// the status and reports it; node-b computes the same refusal but writes
	// no status because only S owns the mapping.
	if hasStatus(b, "games", "a") {
		t.Error("node-b is not the serving node and must not write the mapping's status")
	}
	if !hasStatus(a, "games", "a") {
		t.Fatal("node-a is the serving node and must write the mapping's status")
	}
	want := multiStatusCondition(t, a)
	if want.Status != metav1.ConditionFalse || want.Reason != v1alpha1.ReasonRemotePodMultipleAcceptingNodes {
		t.Errorf("node-a Programmed = %s/%s, want False/RemotePodMultipleAcceptingNodes", want.Status, want.Reason)
	}
	if st := a.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]; st.Endpoint == nil || st.Endpoint.Address != "10.244.7.7" {
		t.Errorf("written status endpoint = %+v, want 10.244.7.7", st.Endpoint)
	}

	// The shared computation, viewpoint by viewpoint: same endpoint, same
	// serving node, same refusal condition everywhere.
	for _, runOn := range []string{"node-a", "node-b", "node-c"} {
		m := resolvedMappingFor(multiAcceptingWorld(runOn), "games", "a")
		if m == nil {
			t.Fatalf("world(%s): mapping not resolved", runOn)
		}
		if m.endpoint == nil || m.endpoint.addr != "10.244.7.7" {
			t.Errorf("world(%s): endpoint = %+v, want 10.244.7.7", runOn, m.endpoint)
		}
		if m.serving != "node-a" {
			t.Errorf("world(%s): serving node = %q, want node-a", runOn, m.serving)
		}
		if m.programmedCond.Status != metav1.ConditionFalse ||
			m.programmedCond.Reason != v1alpha1.ReasonRemotePodMultipleAcceptingNodes {
			t.Errorf("world(%s): Programmed = %s/%s, want False/RemotePodMultipleAcceptingNodes",
				runOn, m.programmedCond.Status, m.programmedCond.Reason)
		}
	}
}

func multiStatusCondition(t *testing.T, res Result) metav1.Condition {
	t.Helper()
	c := programmedOf(res, "games", "a")
	if c.Type == "" {
		t.Fatal("serving node wrote no Programmed condition")
	}
	return c
}

// TestServingNodeIsChosenPodsNode is the F3 case the old code refused outright:
// the class selects node-a and node-b, the only ready pod sits on node-b, the
// second accepting node. node-b is the serving node, programs its local pod,
// and the earlier accepting node is the refused one.
func TestServingNodeIsChosenPodsNode(t *testing.T) {
	world := func(runOn string) Inputs {
		return Inputs{
			NodeName: runOn,
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
				node("node-b", "10.0.0.2", map[string]string{"edge": "true"}),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes: []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"},
				withLinks(claim("node-a", "node-b", 0)))},
			PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.8.8", node: "node-b", target: "pod-alpha"}),
			},
			Now: metav1.NewTime(baseTime),
		}
	}

	b := Compute(world("node-b"))
	if len(b.State.DNAT) != 1 || b.State.DNAT[0].ToAddr.String() != "10.244.8.8" {
		t.Errorf("node-b DNAT = %+v, want one local rule to 10.244.8.8 (S programs its own pod)", b.State.DNAT)
	}
	// A local serve needs no return link, no mark, no rules.
	if len(b.State.Mark) != 0 || len(b.State.Links) != 0 || len(b.State.Rules) != 0 || len(b.State.Routes) != 0 {
		t.Errorf("node-b remote objects = %+v, want none (pod is local to S)", b.State)
	}

	a := Compute(world("node-a"))
	assertCounts(t, a.State, counts{})

	// node-b is S, owns the status, and reports the multi-accepting refusal.
	c := programmedOf(b, "games", "a")
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonRemotePodMultipleAcceptingNodes {
		t.Errorf("node-b Programmed = %s/%s, want False/RemotePodMultipleAcceptingNodes", c.Status, c.Reason)
	}
	if hasStatus(a, "games", "a") {
		t.Error("node-a is not the serving node and must not write status")
	}
}

// windowWorld is a single accepting node (node-a) with the pod on node-b.
// node-a reports a ready class row, so the only thing that can hold the
// mapping back from Programmed is the return-path slot.
func windowWorld(runOn string, links ...v1alpha1.LinkAllocation) Inputs {
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes: []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"},
			withLinks(links...),
			withNodeRows(v1alpha1.NodeStatus{Name: "node-a", Ready: true}))},
		PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-b", target: "pod-a"}),
		},
		Now: metav1.NewTime(baseTime),
	}
}

// TestClaimWindowReportsHonest covers the F2/F9 window: while the return-path
// claim has not landed, the holder emits no reply-mark rule (an unresolved
// slot must never become mark 0x6b700000) and the serving node does not call
// the mapping programmed. Once the claim lands, both flip.
func TestClaimWindowReportsHonest(t *testing.T) {
	t.Run("unlanded claim", func(t *testing.T) {
		holder := Compute(windowWorld("node-b"))
		if len(holder.State.Mark) != 0 {
			t.Errorf("holder Mark = %+v, want none until the claim lands", holder.State.Mark)
		}
		if len(holder.State.Links) != 0 || len(holder.State.Rules) != 0 || len(holder.State.Routes) != 0 {
			t.Errorf("holder emitted link-dependent objects %+v during the window", holder.State)
		}
		c := programmedOf(Compute(windowWorld("node-a")), "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonReturnPathUnavailable {
			t.Errorf("Programmed during window = %s/%s, want False/ReturnPathUnavailable", c.Status, c.Reason)
		}
	})

	t.Run("landed claim", func(t *testing.T) {
		links := []v1alpha1.LinkAllocation{claim("node-a", "node-b", 0)}
		holder := Compute(windowWorld("node-b", links...))
		if len(holder.State.Mark) != 1 || holder.State.Mark[0].Mark != markBase {
			t.Errorf("holder Mark = %+v, want one rule at slot 0", holder.State.Mark)
		}
		if len(holder.State.Rules) != 2 || len(holder.State.Routes) != 1 {
			t.Errorf("holder rules/routes = %d/%d, want 2/1 once the slot is resolved",
				len(holder.State.Rules), len(holder.State.Routes))
		}
		c := programmedOf(Compute(windowWorld("node-a", links...)), "games", "a")
		if c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonAllNodesReady {
			t.Errorf("Programmed after landing = %s/%s, want True/AllNodesReady", c.Status, c.Reason)
		}
	})
}

// counts is an expected tally of a State's slices.
type counts struct {
	dnat, exempt, mark, links, rules, routes int
}

func assertCounts(t *testing.T, st datapath.State, want counts) {
	t.Helper()
	got := counts{
		dnat:   len(st.DNAT),
		exempt: len(st.Exempt),
		mark:   len(st.Mark),
		links:  len(st.Links),
		rules:  len(st.Rules),
		routes: len(st.Routes),
	}
	if got != want {
		t.Fatalf("state counts = %+v, want %+v", got, want)
	}
}

// richWorld is a fixture that exercises every output path: co-located and remote
// mappings, a conflict, a missing class, a live claim, a claim to garbage
// collect and one to drop, across two classes. Computed from node-a.
func richWorld() Inputs {
	oldUnused := metav1.NewTime(baseTime.Add(-48 * time.Hour)) // 48h before now
	return Inputs{
		NodeName: "node-a",
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", map[string]string{"internal": "true"}),
			node("node-c", "10.0.0.3", nil),
			node("node-d", "10.0.0.4", nil),
		},
		Namespaces: []*corev1.Namespace{
			ns("games", map[string]string{"public": "yes"}),
			ns("infra", map[string]string{"public": "yes"}),
		},
		Classes: []*v1alpha1.PortMapClass{
			class("public", map[string]string{"edge": "true"},
				withInterfaces("eth0", "eth1"),
				withLinks(
					claim("node-a", "node-c", 0),                   // needed by web -> node-c
					claim("node-a", "node-d", 1),                   // not needed -> set UnusedSince
					unusedClaim("node-b", "node-d", 2, &oldUnused), // stale -> drop
				),
			),
			class("internal", map[string]string{"internal": "true"}),
		},
		PortMaps: []*v1alpha1.PortMap{
			pm("games", "web", 3000, 0, withService("web", "svc")),
			pm("games", "db", 5000, 0, withService("db", "svc")),
			pm("games", "conflict-a", 8000, 0, withEndPort(8100)),
			pm("games", "conflict-b", 8050, 5),
			pm("games", "missing", 9000, 0, withClass("nope")),
			pm("infra", "cache", 6000, 0, withClass("internal"), withService("cache", "svc")),
		},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "web", endpointSpec{addr: "10.244.3.3", node: "node-c", target: "pod-web"}),
			slice("games", "db", endpointSpec{addr: "10.244.0.7", node: "node-a", target: "pod-db"}),
			slice("games", "conflict-a", endpointSpec{addr: "10.244.0.8", node: "node-a", target: "pod-ca"}),
			slice("infra", "cache", endpointSpec{addr: "10.244.1.4", node: "node-b", target: "pod-cache"}),
		},
		Now: metav1.NewTime(baseTime),
	}
}

// unusedClaim is a claim carrying an UnusedSince timestamp.
func unusedClaim(a, b string, slot int32, since *metav1.Time) v1alpha1.LinkAllocation {
	c := claim(a, b, slot)
	c.UnusedSince = since
	return c
}

// cloneInputs copies the top-level slices so a shuffle never reorders the base.
// Element pointers are shared; Compute treats them read-only.
func cloneInputs(in Inputs) Inputs {
	out := in
	out.Nodes = append([]*corev1.Node(nil), in.Nodes...)
	out.Namespaces = append([]*corev1.Namespace(nil), in.Namespaces...)
	out.Classes = append([]*v1alpha1.PortMapClass(nil), in.Classes...)
	out.PortMaps = append([]*v1alpha1.PortMap(nil), in.PortMaps...)
	out.Slices = append([]*discoveryv1.EndpointSlice(nil), in.Slices...)
	return out
}

// shuffleInputs reorders every input slice in place with the given RNG.
func shuffleInputs(in Inputs, rng *rand.Rand) Inputs {
	rng.Shuffle(len(in.Nodes), func(i, j int) { in.Nodes[i], in.Nodes[j] = in.Nodes[j], in.Nodes[i] })
	rng.Shuffle(len(in.Namespaces), func(i, j int) { in.Namespaces[i], in.Namespaces[j] = in.Namespaces[j], in.Namespaces[i] })
	rng.Shuffle(len(in.Classes), func(i, j int) { in.Classes[i], in.Classes[j] = in.Classes[j], in.Classes[i] })
	rng.Shuffle(len(in.PortMaps), func(i, j int) { in.PortMaps[i], in.PortMaps[j] = in.PortMaps[j], in.PortMaps[i] })
	rng.Shuffle(len(in.Slices), func(i, j int) { in.Slices[i], in.Slices[j] = in.Slices[j], in.Slices[i] })
	return in
}
