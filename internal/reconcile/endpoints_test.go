package reconcile

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// dnatAddrs returns the DNAT target addresses in a Result's state.
func dnatAddrs(res Result) []string {
	var out []string
	for _, r := range res.State.DNAT {
		out = append(out, r.ToAddr.String())
	}
	return out
}

func programmedOf(res Result, namespace, name string) metav1.Condition {
	st := res.PortMapStatus[types.NamespacedName{Namespace: namespace, Name: name}]
	return findCond(st.Conditions, v1alpha1.ConditionProgrammed)
}

func TestClassSelection(t *testing.T) {
	twoNodes := []*corev1.Node{
		node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
		node("node-b", "10.0.0.2", nil),
	}
	tests := []struct {
		name      string
		nodeMatch map[string]string
		nodes     []*corev1.Node
		runOn     string
		wantDNAT  bool
	}{
		{name: "selects this node", nodeMatch: map[string]string{"edge": "true"}, nodes: twoNodes, runOn: "node-a", wantDNAT: true},
		{name: "does not select this node", nodeMatch: map[string]string{"edge": "true"}, nodes: twoNodes, runOn: "node-b", wantDNAT: false},
		// An empty selector matches every node; with node-b the only node it is
		// the sole accepting node and, hosting the pod, is co-located.
		{name: "empty nodeSelector selects everything", nodeMatch: map[string]string{},
			nodes: []*corev1.Node{node("node-b", "10.0.0.2", nil)}, runOn: "node-b", wantDNAT: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Inputs{
				NodeName:   tt.runOn,
				Nodes:      tt.nodes,
				Namespaces: []*corev1.Namespace{ns("games", nil)},
				Classes:    []*v1alpha1.PortMapClass{class("public", tt.nodeMatch)},
				PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
				// The pod is on whichever node runs, so it is a local candidate
				// and the mapping is co-located: DNAT with no return link.
				Slices: []*discoveryv1.EndpointSlice{
					slice("games", "a", endpointSpec{addr: "10.244.0.5", node: tt.runOn, target: "pod-a"}),
				},
				Now: metav1.NewTime(baseTime),
			}
			got := len(dnatAddrs(Compute(in))) > 0
			if got != tt.wantDNAT {
				t.Fatalf("has DNAT = %v, want %v", got, tt.wantDNAT)
			}
		})
	}
}

func TestEndpointChoice(t *testing.T) {
	// A public class on a single accepting node, node-a. Endpoint placement and
	// readiness vary per case. Compute runs on node-a, so the chosen endpoint
	// shows up as node-a's DNAT target.
	build := func(runOn string, eps ...endpointSpec) Inputs {
		return Inputs{
			NodeName: runOn,
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
				node("node-b", "10.0.0.2", nil),
				node("node-c", "10.0.0.3", nil),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes:    []*v1alpha1.PortMapClass{class("public", map[string]string{"edge": "true"})},
			PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
			Slices:     []*discoveryv1.EndpointSlice{slice("games", "a", eps...)},
			Now:        metav1.NewTime(baseTime),
		}
	}

	t.Run("pod on the accepting node wins", func(t *testing.T) {
		// node-a is the only accepting node and hosts pod-z; the lower-named
		// pod-a sits on non-accepting node-b. The accepting-node rank wins
		// over the name tiebreak — deterministically, on every agent.
		in := build("node-a",
			endpointSpec{addr: "10.244.1.1", node: "node-b", target: "pod-a"},
			endpointSpec{addr: "10.244.0.9", node: "node-a", target: "pod-z"},
		)
		if got := dnatAddrs(Compute(in)); len(got) != 1 || got[0] != "10.244.0.9" {
			t.Fatalf("DNAT = %v, want [10.244.0.9] (pod on the accepting node)", got)
		}
	})

	t.Run("no local pod picks lowest targetRef.name", func(t *testing.T) {
		in := build("node-a",
			endpointSpec{addr: "10.244.2.2", node: "node-c", target: "pod-y"},
			endpointSpec{addr: "10.244.1.1", node: "node-b", target: "pod-a"},
		)
		if got := dnatAddrs(Compute(in)); len(got) != 1 || got[0] != "10.244.1.1" {
			t.Fatalf("DNAT = %v, want [10.244.1.1] (lowest targetRef.name pod-a)", got)
		}
	})

	t.Run("unready endpoints skipped", func(t *testing.T) {
		in := build("node-a",
			endpointSpec{addr: "10.244.1.1", node: "node-b", target: "pod-a", notReady: true},
			endpointSpec{addr: "10.244.2.2", node: "node-c", target: "pod-b"},
		)
		if got := dnatAddrs(Compute(in)); len(got) != 1 || got[0] != "10.244.2.2" {
			t.Fatalf("DNAT = %v, want [10.244.2.2] (only ready endpoint)", got)
		}
	})

	t.Run("nil ready treated as ready", func(t *testing.T) {
		in := build("node-a",
			endpointSpec{addr: "10.244.3.3", node: "node-b", target: "pod-a"}, // ready nil
		)
		if got := dnatAddrs(Compute(in)); len(got) != 1 || got[0] != "10.244.3.3" {
			t.Fatalf("DNAT = %v, want [10.244.3.3] (nil ready is ready)", got)
		}
	})

	t.Run("zero candidates emits no DNAT and refuses", func(t *testing.T) {
		in := build("node-a",
			endpointSpec{addr: "10.244.1.1", node: "node-b", target: "pod-a", notReady: true},
		)
		res := Compute(in)
		if got := dnatAddrs(res); len(got) != 0 {
			t.Fatalf("DNAT = %v, want none", got)
		}
		c := programmedOf(res, "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonNoReadyEndpoint {
			t.Fatalf("Programmed = %s/%s, want False/NoReadyEndpoint", c.Status, c.Reason)
		}
	})
}
