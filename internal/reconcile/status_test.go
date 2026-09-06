package reconcile

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

func hasStatus(res Result, namespace, name string) bool {
	_, ok := res.PortMapStatus[types.NamespacedName{Namespace: namespace, Name: name}]
	return ok
}

func TestStatusOwnedByServingNode(t *testing.T) {
	// node-a accepts, the pod is on node-b. Only node-a, the serving node
	// every agent computes identically, writes the mapping's status; the
	// endpoint holder reads it, it does not write it.
	world := func(runOn string) Inputs {
		return Inputs{
			NodeName: runOn,
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
				node("node-b", "10.0.0.2", nil),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes: []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"},
				withLinks(claim("node-a", "node-b", 0)))},
			PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-b", target: "pod-a"}),
			},
			Now: metav1.NewTime(baseTime),
		}
	}

	if hasStatus(Compute(world("node-b")), "games", "a") {
		t.Error("node-b holds the endpoint but is not the serving node and must not write status")
	}
	res := Compute(world("node-a"))
	if !hasStatus(res, "games", "a") {
		t.Fatal("node-a is the serving node and must write status")
	}
	st := res.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]
	if st.Endpoint == nil || st.Endpoint.Node != "node-b" || st.Endpoint.Address != "10.244.5.5" {
		t.Errorf("endpoint = %+v, want node-b/10.244.5.5", st.Endpoint)
	}
}

func TestPublishedRows(t *testing.T) {
	// Two accepting nodes, two interfaces: one Published row per accepting
	// node and interface. The pod is remote and on neither accepting node,
	// so the first accepting node by name, node-a, is the serving node and
	// the status writer.
	in := Inputs{
		NodeName: "node-a",
		Nodes: []*corev1.Node{
			node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
			node("node-b", "10.0.0.2", map[string]string{"edge": "true"}),
			node("node-c", "10.0.0.3", nil),
		},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes: []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"},
			withInterfaces("eth0", "eth1"))},
		PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0, withGeneration(7))},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.9.9", node: "node-c", target: "pod-a"}),
		},
		Now: metav1.NewTime(baseTime),
	}

	st := Compute(in).PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]
	want := []v1alpha1.PublishedAddress{
		{Node: "node-a", Interface: "eth0"},
		{Node: "node-a", Interface: "eth1"},
		{Node: "node-b", Interface: "eth0"},
		{Node: "node-b", Interface: "eth1"},
	}
	if len(st.Published) != len(want) {
		t.Fatalf("published rows = %d, want %d: %+v", len(st.Published), len(want), st.Published)
	}
	for i, w := range want {
		got := st.Published[i]
		if got.Node != w.Node || got.Interface != w.Interface || got.Address != "" {
			t.Errorf("row %d = %+v, want %+v with empty address", i, got, w)
		}
	}
	if st.ObservedGeneration != 7 {
		t.Errorf("observedGeneration = %d, want 7", st.ObservedGeneration)
	}
}

// coopWorld is a single accepting node (node-a) hosting the chosen pod: the
// co-located shape where node-a is the serving node and status owner. Class
// node rows vary per subtest; they are the only thing the Programmed gate can
// have to go on here.
func coopWorld(rows ...v1alpha1.NodeStatus) Inputs {
	opts := []classOpt{}
	if rows != nil {
		opts = append(opts, withNodeRows(rows...))
	}
	return Inputs{
		NodeName:   "node-a",
		Nodes:      []*corev1.Node{node("node-a", "10.0.0.1", map[string]string{"edge": "true"})},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes:    []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"}, opts...)},
		PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.0.9", node: "node-a", target: "pod-a"}),
		},
		Now: metav1.NewTime(baseTime),
	}
}

// TestProgrammedGatesOnNodeRows is the F2 gate: Programmed=True with reason
// AllNodesReady only when every accepting node for the class reports a ready
// row. A missing or not-ready row is NodeNotReady, its message naming the
// node and what the row said.
func TestProgrammedGatesOnNodeRows(t *testing.T) {
	t.Run("row missing", func(t *testing.T) {
		c := programmedOf(Compute(coopWorld()), "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonNodeNotReady {
			t.Fatalf("Programmed = %s/%s, want False/NodeNotReady while node-a has not reported", c.Status, c.Reason)
		}
		if !strings.Contains(c.Message, "node-a") {
			t.Errorf("message %q does not name the node", c.Message)
		}
	})

	t.Run("row not ready", func(t *testing.T) {
		in := coopWorld(v1alpha1.NodeStatus{Name: "node-a", Ready: false,
			Message: "interface wt0 not present"})
		c := programmedOf(Compute(in), "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonNodeNotReady {
			t.Fatalf("Programmed = %s/%s, want False/NodeNotReady", c.Status, c.Reason)
		}
		if !strings.Contains(c.Message, "node-a") || !strings.Contains(c.Message, "wt0") {
			t.Errorf("message %q must name the node and carry the row's reason", c.Message)
		}
	})

	t.Run("every row ready", func(t *testing.T) {
		in := coopWorld(v1alpha1.NodeStatus{Name: "node-a", Ready: true})
		c := programmedOf(Compute(in), "games", "a")
		if c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonAllNodesReady {
			t.Fatalf("Programmed = %s/%s, want True/AllNodesReady", c.Status, c.Reason)
		}
	})
}

// TestNodeRowHonestReadiness is the F2 host self-check: the class row's
// Ready reflects whether every interface the class selects resolves to an
// address on this node (as read by the agent through its host seam and
// handed in as InterfaceAddrs). A missing interface is a message naming it,
// not a silently skipped row.
func TestNodeRowHonestReadiness(t *testing.T) {
	t.Run("missing interface", func(t *testing.T) {
		in := coopWorld()
		in.Classes[0].Spec.Interfaces = []string{"eth0", "wt0"}
		in.InterfaceAddrs = map[string]string{"eth0": "10.0.0.1"}
		row := Compute(in).ClassStatus["public"].Node
		if row.Ready {
			t.Error("row Ready = true with an unresolvable interface")
		}
		if row.Message != "interface wt0 not present" {
			t.Errorf("message = %q, want %q", row.Message, "interface wt0 not present")
		}
		if row.Addresses["eth0"] != "10.0.0.1" {
			t.Errorf("addresses = %v, want eth0 resolved", row.Addresses)
		}
		if _, ok := row.Addresses["wt0"]; ok {
			t.Errorf("addresses carry an unresolved interface: %v", row.Addresses)
		}
	})

	t.Run("every interface resolves", func(t *testing.T) {
		in := coopWorld()
		in.Classes[0].Spec.Interfaces = []string{"eth0", "wt0"}
		in.InterfaceAddrs = map[string]string{"eth0": "10.0.0.1", "wt0": "100.64.0.7"}
		row := Compute(in).ClassStatus["public"].Node
		if !row.Ready || row.Message != "" {
			t.Errorf("row = %+v, want ready with no message", row)
		}
		if row.Addresses["wt0"] != "100.64.0.7" {
			t.Errorf("addresses = %v, want wt0 resolved", row.Addresses)
		}
	})
}
