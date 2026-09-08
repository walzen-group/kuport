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
		Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a"}, allOpts...)},
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
			Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a"})},
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
				Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a", "node-b"},
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
			t.Errorf("node-b DNAT = %d, want 0 (the accepting sibling serves nothing)", got)
		}
		// The 2026-09-06 adjudication retires the multi-accepting refusal:
		// this mapping is served, not refused. node-a (S) owns the status and
		// gates on the participants; no class node row is seeded here, so the
		// honest reading is False/NodeNotReady naming S, not a refusal and not
		// True. The endpoint holder contributes only its half of the return path.
		c := programmedOf(Compute(build("node-a")), "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonNodeNotReady {
			t.Fatalf("Programmed = %s/%s, want False/NodeNotReady while node-a has not reported", c.Status, c.Reason)
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
			Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a"},
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

// multiAcceptingWorld is the F1 shape under the adjudicated semantics: a
// Service with ready endpoints on two accepting nodes (node-a, node-b) and a
// claim between them. The deterministic rule puts the chosen endpoint on the
// lexicographically first accepting node (node-a, whose pod is named higher),
// so node-a serves locally and no return link is needed. The class status
// carries S's ready row and the accepting sibling's not-ready row: under the
// 2026-09-06 adjudication (a served mapping reports the truth about itself)
// the sibling gates nothing, so every viewpoint settles on Programmed=True.
func multiAcceptingWorld(runOn string) Inputs {
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", map[string]string{"edge": "true"}),
			node("node-c", "10.0.0.3", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a", "node-b"},
			withLinks(claim("node-a", "node-b", 0)),
			withNodeRows(
				v1alpha1.NodeStatus{Name: "node-a", Ready: true},
				v1alpha1.NodeStatus{Name: "node-b", Ready: false, Message: "interface eth1 not present"},
			))},
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

// TestAgentAgreementMultiAccepting extends the TestAgentAgreement shape to
// the F1 fixture under the 2026-09-06 adjudication: every viewpoint must pick
// the same endpoint (the one on node-a, the first accepting node, even though
// node-b's pod is named lower), exactly one node programs, the accepting
// sibling contributes no rules and gates nothing, and only the serving node
// owns the PortMap status.
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

	// The served reading is the shared computation: node-a owns the status
	// and reports True on its ready row and local pod; node-b computes the
	// same verdict but writes no status because only S owns the mapping.
	if hasStatus(b, "games", "a") {
		t.Error("node-b is not the serving node and must not write the mapping's status")
	}
	if !hasStatus(a, "games", "a") {
		t.Fatal("node-a is the serving node and must write the mapping's status")
	}
	want := multiStatusCondition(t, a)
	if want.Status != metav1.ConditionTrue || want.Reason != v1alpha1.ReasonAllNodesReady {
		t.Errorf("node-a Programmed = %s/%s, want True/AllNodesReady (a served mapping reports the truth about itself)", want.Status, want.Reason)
	}
	if st := a.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]; st.Endpoint == nil || st.Endpoint.Address != "10.244.7.7" {
		t.Errorf("written status endpoint = %+v, want 10.244.7.7", st.Endpoint)
	}

	// The shared computation, viewpoint by viewpoint: same endpoint, same
	// serving node, same Programmed condition everywhere.
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
		if m.programmedCond.Status != metav1.ConditionTrue ||
			m.programmedCond.Reason != v1alpha1.ReasonAllNodesReady {
			t.Errorf("world(%s): Programmed = %s/%s, want True/AllNodesReady",
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
// and the earlier accepting node serves nothing. Under the 2026-09-06
// adjudication node-b's served mapping reads True on its own ready row; the
// sibling node-a reports nothing and gates nothing.
func TestServingNodeIsChosenPodsNode(t *testing.T) {
	world := func(runOn string) Inputs {
		return Inputs{
			NodeName: runOn,
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
				node("node-b", "10.0.0.2", map[string]string{"edge": "true"}),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a", "node-b"},
				withLinks(claim("node-a", "node-b", 0)),
				withNodeRows(v1alpha1.NodeStatus{Name: "node-b", Ready: true}))},
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

	// node-b is S, owns the status, and reports the served mapping True.
	c := programmedOf(b, "games", "a")
	if c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonAllNodesReady {
		t.Errorf("node-b Programmed = %s/%s, want True/AllNodesReady (a served mapping reports the truth about itself)", c.Status, c.Reason)
	}
	if hasStatus(a, "games", "a") {
		t.Error("node-a is not the serving node and must not write status")
	}
}

// TestMultiAcceptRemotePodWindow is the adjudicated remote case: the class
// selects two accepting nodes (node-a, node-b), the chosen pod sits on a
// third (node-c), and node-a — the first accepting node — serves across the
// return path. Before the S-holder claim lands the mapping reports honestly
// (not True; ReturnPathUnavailable, task-8's window reason); once it lands
// it reads True/AllNodesReady while the sibling node-b reports no row and
// gates nothing.
func TestMultiAcceptRemotePodWindow(t *testing.T) {
	world := func(runOn string, links ...v1alpha1.LinkAllocation) Inputs {
		return Inputs{
			NodeName: runOn,
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
				node("node-b", "10.0.0.2", map[string]string{"edge": "true"}),
				node("node-c", "10.0.0.3", nil),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a", "node-b"},
				withLinks(links...),
				// Only the participant S reports a row; the sibling is absent.
				withNodeRows(v1alpha1.NodeStatus{Name: "node-a", Ready: true}))},
			PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.9.9", node: "node-c", target: "pod-a"}),
			},
			Now: metav1.NewTime(baseTime),
		}
	}

	t.Run("unlanded claim", func(t *testing.T) {
		c := programmedOf(Compute(world("node-a")), "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonReturnPathUnavailable {
			t.Errorf("window Programmed = %s/%s, want False/ReturnPathUnavailable", c.Status, c.Reason)
		}
	})

	t.Run("landed claim", func(t *testing.T) {
		links := []v1alpha1.LinkAllocation{claim("node-a", "node-c", 0)}
		res := Compute(world("node-a", links...))
		c := programmedOf(res, "games", "a")
		if c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonAllNodesReady {
			t.Errorf("after landing Programmed = %s/%s, want True/AllNodesReady", c.Status, c.Reason)
		}
		if len(res.State.DNAT) != 1 || len(res.State.Links) != 1 {
			t.Errorf("node-a state = %+v, want DNAT plus the return link", res.State)
		}
	})
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
		Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a"},
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
			class("public", []string{"node-a"},
				withInterfaces("eth0", "eth1"),
				withLinks(
					claim("node-a", "node-c", 0),                   // needed by web -> node-c
					claim("node-a", "node-d", 1),                   // not needed -> set UnusedSince
					unusedClaim("node-b", "node-d", 2, &oldUnused), // stale -> drop
				),
			),
			class("internal", []string{"node-e"}),
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

// multiWorld is a three-worker class in Multi serving with the pod on node-c,
// so every accepting node forwards and two of them need a return link. Claims
// for both pairs are present so the links can build at known slots.
func multiWorld(runOn string, opts ...classOpt) Inputs {
	all := append([]classOpt{
		withMultiServing(),
		withLinks(claim("node-a", "node-c", 0), claim("node-b", "node-c", 1)),
		withNodeRows(
			v1alpha1.NodeStatus{Name: "node-a", Ready: true},
			v1alpha1.NodeStatus{Name: "node-b", Ready: true},
			v1alpha1.NodeStatus{Name: "node-c", Ready: true},
		),
	}, opts...)
	return Inputs{
		NodeName: runOn,
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", nil),
			node("node-b", "10.0.0.2", nil),
			node("node-c", "10.0.0.3", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a", "node-b", "node-c"}, all...)},
		PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.9.9", node: "node-c", target: "pod-a"}),
		},
		Now: metav1.NewTime(baseTime),
	}
}

// TestMultiServingEveryAcceptingNodeForwards is the point of the mode: a routed
// virtual address may land on any accepting node, so every one of them must
// carry the DNAT rules rather than only the serving node.
func TestMultiServingEveryAcceptingNodeForwards(t *testing.T) {
	// A node holding no pod translates to the far end of its own return link,
	// so the target learns which node forwarded from the device the request
	// arrives on. The CNI's tunnel carries nothing that says which.
	for _, n := range []string{"node-a", "node-b"} {
		res := Compute(multiWorld(n))
		if len(res.State.DNAT) != 1 {
			t.Errorf("%s: DNAT = %+v, want one rule (every accepting node forwards)", n, res.State.DNAT)
			continue
		}
		if len(res.State.Links) != 1 {
			t.Fatalf("%s: Links = %+v, want the one return link the DNAT points at", n, res.State.Links)
		}
		got := res.State.DNAT[0].ToAddr
		if got.String() == "10.244.9.9" {
			t.Errorf("%s: DNAT still points at the pod, which the CNI's tunnel cannot attribute", n)
		}
		if !got.IsLinkLocalUnicast() {
			t.Errorf("%s: DNAT target = %s, want the peer's end of the return link", n, got)
		}
		// The two ends of a /31 differ in the low bit alone.
		mine := res.State.Links[0].LinkAddr.Addr()
		if got != mine.Next() && got != mine.Prev() {
			t.Errorf("%s: DNAT target = %s, want the far end of %s", n, got, res.State.Links[0].LinkAddr)
		}
	}

	// The pod's node translates its own interfaces straight to the pod, and
	// translates each link to the pod as well, so a forwarded request lands.
	res := Compute(multiWorld("node-c"))
	if len(res.State.DNAT) != 3 {
		t.Fatalf("node-c: DNAT = %+v, want its own interface plus one per return link", res.State.DNAT)
	}
	viaLink := 0
	for _, d := range res.State.DNAT {
		if d.ToAddr.String() != "10.244.9.9" {
			t.Errorf("node-c: DNAT target = %s, want the pod 10.244.9.9", d.ToAddr)
		}
		if d.DstAddr == nil {
			continue
		}
		viaLink++
		// A link rule matches this node's own end of that link, which keeps it
		// off anything else that arrives on the device.
		if !d.DstAddr.IsLinkLocalUnicast() {
			t.Errorf("node-c: link DNAT matches %s, want this node's link address", d.DstAddr)
		}
	}
	if viaLink != 2 {
		t.Errorf("node-c: %d link-matched DNAT rules, want one per remote programmer", viaLink)
	}
}

// TestMultiServingUsesConntrackReturn covers the reason Multi needs conntrack:
// the pod's node holds a link per remote programmer, and a static mark rule
// cannot say which one a reply belongs to. It saves the peer's mark per link on
// the way in and restores it on the way out.
func TestMultiServingUsesConntrackReturn(t *testing.T) {
	res := Compute(multiWorld("node-c"))

	if len(res.State.Mark) != 0 {
		t.Errorf("Mark = %+v, want none under Multi (a static mark cannot tell peers apart)", res.State.Mark)
	}
	if len(res.State.CtSave) != 2 {
		t.Fatalf("CtSave = %+v, want one per remote programmer (node-a, node-b)", res.State.CtSave)
	}
	if len(res.State.CtLoad) != 1 {
		t.Fatalf("CtLoad = %+v, want one restore rule for the pod's replies", res.State.CtLoad)
	}

	// Each save names a distinct link and a distinct mark, which is what makes
	// the reply leave by the link its request arrived on.
	seenIface := map[string]bool{}
	seenMark := map[uint32]bool{}
	for _, c := range res.State.CtSave {
		if seenIface[c.Iface] {
			t.Errorf("CtSave repeats interface %s; each peer needs its own link", c.Iface)
		}
		if seenMark[c.Mark] {
			t.Errorf("CtSave repeats mark 0x%x; each peer needs its own slot", c.Mark)
		}
		seenIface[c.Iface] = true
		seenMark[c.Mark] = true
	}

	if len(res.State.Links) != 2 {
		t.Fatalf("Links = %d, want one per remote programmer", len(res.State.Links))
	}
	// The kernel keys a VXLAN device by VNI and UDP port, so two links on one
	// node sharing both is refused with EEXIST and the whole apply fails. Only
	// a cluster showed this; the counts above all passed while it was broken.
	if a, b := res.State.Links[0], res.State.Links[1]; a.VNI == b.VNI && a.Port == b.Port {
		t.Errorf("links %s and %s share vni %d on port %d; the kernel refuses the second",
			a.Name, b.Name, a.VNI, a.Port)
	}
	if len(res.State.Routes) != 2 {
		t.Errorf("Routes = %d, want one return route per peer", len(res.State.Routes))
	}
	// A loop guard and a divert rule per peer.
	if len(res.State.Rules) != 4 {
		t.Errorf("Rules = %d, want a loop guard and a divert per peer", len(res.State.Rules))
	}
}

// TestSingleServingKeepsStaticMark pins the default: nothing about the
// conntrack path appears for a class that did not ask for Multi.
func TestSingleServingKeepsStaticMark(t *testing.T) {
	res := Compute(remoteWorld("node-b"))
	if len(res.State.Mark) != 1 {
		t.Errorf("Mark = %+v, want the one static rule Single has always used", res.State.Mark)
	}
	if len(res.State.CtSave) != 0 || len(res.State.CtLoad) != 0 {
		t.Errorf("CtSave/CtLoad = %+v/%+v, want none under Single", res.State.CtSave, res.State.CtLoad)
	}
}

// TestMultiServingPublishesEveryNode: the addresses a client may dial are every
// programmer's, which is what makes a routed virtual address usable.
func TestMultiServingPublishesEveryNode(t *testing.T) {
	res := Compute(multiWorld("node-c"))
	st, ok := res.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]
	if !ok {
		t.Fatal("no status written for the mapping")
	}
	nodes := map[string]bool{}
	for _, p := range st.Published {
		nodes[p.Node] = true
	}
	for _, want := range []string{"node-a", "node-b", "node-c"} {
		if !nodes[want] {
			t.Errorf("published rows = %+v, want one naming %s", st.Published, want)
		}
	}
}
