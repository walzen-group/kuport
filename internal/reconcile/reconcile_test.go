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
		// The endpoint holder (node-b) owns the status and reports the reason.
		holder := Compute(remoteWorld("node-b", withMode(v1alpha1.ReturnPathNone)))
		c := programmedOf(holder, "games", "a")
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
		// Only the first accepting node by name (node-a) programs.
		if got := len(Compute(build("node-a")).State.DNAT); got != 1 {
			t.Errorf("node-a DNAT = %d, want 1 (first accepting node programs)", got)
		}
		if got := len(Compute(build("node-b")).State.DNAT); got != 0 {
			t.Errorf("node-b DNAT = %d, want 0 (second accepting node stays out)", got)
		}
		// The endpoint holder (node-c) reports the limitation.
		c := programmedOf(Compute(build("node-c")), "games", "a")
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
	if _, ok := b.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]; !ok {
		t.Errorf("endpoint holder node-b should own the PortMap status")
	}
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
