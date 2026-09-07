package reconcile

import (
	"reflect"
	"sort"
	"strings"
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

// TestPerNodeInterfaces is the point of the class naming each node's
// interfaces: one class covers nodes whose NICs are named differently, and one
// mapping's interface list resolves against each node's own set. Asking for
// wt0, enp1s0 and eth0 binds wt0 on both nodes, enp1s0 only where it exists,
// and eth0 only where that exists.
func TestPerNodeInterfaces(t *testing.T) {
	world := func(runOn string, pmOpts ...pmOpt) Inputs {
		return Inputs{
			NodeName: runOn,
			Nodes: []*corev1.Node{
				node("node-a", "10.0.0.1", nil),
				node("node-b", "10.0.0.2", nil),
			},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a", "node-b"},
				withNodeInterfaces("node-a", "wt0", "enp1s0"),
				withNodeInterfaces("node-b", "wt0", "eth0"))},
			PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0, pmOpts...)},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.0.5", node: runOn, target: "pod-a"}),
			},
			Now: metav1.NewTime(baseTime),
		}
	}

	ifacesOf := func(res Result) []string {
		var out []string
		for _, r := range res.State.DNAT {
			out = append(out, r.Iface)
		}
		sort.Strings(out)
		return out
	}

	tests := []struct {
		name  string
		opts  []pmOpt
		nodeA []string
		nodeB []string
	}{
		{
			name:  "no list takes every interface the class gives the node",
			nodeA: []string{"enp1s0", "wt0"},
			nodeB: []string{"eth0", "wt0"},
		},
		{
			name:  "one list spans nodes with different NIC names",
			opts:  []pmOpt{withPMInterfaces("wt0", "enp1s0", "eth0")},
			nodeA: []string{"enp1s0", "wt0"},
			nodeB: []string{"eth0", "wt0"},
		},
		{
			name:  "intranet only, the interface both nodes carry",
			opts:  []pmOpt{withPMInterfaces("wt0")},
			nodeA: []string{"wt0"},
			nodeB: []string{"wt0"},
		},
		{
			name:  "a name only one node carries binds on that node alone",
			opts:  []pmOpt{withPMInterfaces("enp1s0")},
			nodeA: []string{"enp1s0"},
			nodeB: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ifacesOf(Compute(world("node-a", tt.opts...))); !reflect.DeepEqual(got, tt.nodeA) {
				t.Errorf("node-a interfaces = %v, want %v", got, tt.nodeA)
			}
			if got := ifacesOf(Compute(world("node-b", tt.opts...))); !reflect.DeepEqual(got, tt.nodeB) {
				t.Errorf("node-b interfaces = %v, want %v", got, tt.nodeB)
			}
		})
	}
}

// TestInterfaceNotInClass rejects a mapping naming an interface no node of the
// class carries. A name some node carries is the whole point of the field and
// is accepted.
func TestInterfaceNotInClass(t *testing.T) {
	world := func(pmOpts ...pmOpt) Inputs {
		return Inputs{
			NodeName:   "node-a",
			Nodes:      []*corev1.Node{node("node-a", "10.0.0.1", nil)},
			Namespaces: []*corev1.Namespace{ns("games", nil)},
			Classes: []*v1alpha1.PortMapClass{class("public", []string{"node-a"},
				withInterfaces("wt0", "enp1s0"))},
			PortMaps: []*v1alpha1.PortMap{pm("games", "a", 3000, 0, pmOpts...)},
			Slices: []*discoveryv1.EndpointSlice{
				slice("games", "a", endpointSpec{addr: "10.244.0.5", node: "node-a", target: "pod-a"}),
			},
			Now: metav1.NewTime(baseTime),
		}
	}

	t.Run("unknown name is rejected", func(t *testing.T) {
		res := Compute(world(withPMInterfaces("wt0", "typo0")))
		c := acceptedOf(res, "games", "a")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonInterfaceNotInClass {
			t.Fatalf("Accepted = %s/%s, want False/InterfaceNotInClass", c.Status, c.Reason)
		}
		if !strings.Contains(c.Message, "typo0") {
			t.Errorf("message = %q, want it to name typo0", c.Message)
		}
	})

	t.Run("known name is accepted", func(t *testing.T) {
		res := Compute(world(withPMInterfaces("enp1s0")))
		if c := acceptedOf(res, "games", "a"); c.Status != metav1.ConditionTrue {
			t.Fatalf("Accepted = %s/%s, want True", c.Status, c.Reason)
		}
	})
}

func TestClassSelection(t *testing.T) {
	twoNodes := []*corev1.Node{
		node("node-a", "10.0.0.1", map[string]string{"edge": "true"}),
		node("node-b", "10.0.0.2", nil),
	}
	tests := []struct {
		name       string
		classNodes []string
		nodes      []*corev1.Node
		runOn      string
		wantDNAT   bool
	}{
		{name: "names this node", classNodes: []string{"node-a"}, nodes: twoNodes, runOn: "node-a", wantDNAT: true},
		{name: "does not name this node", classNodes: []string{"node-a"}, nodes: twoNodes, runOn: "node-b", wantDNAT: false},
		// A class naming both nodes accepts on each; with the pod on the node
		// that runs, the mapping is co-located.
		{name: "names every node", classNodes: []string{"node-a", "node-b"},
			nodes: twoNodes, runOn: "node-b", wantDNAT: true},
		// A name with no Node object is skipped, so the class accepts nowhere.
		{name: "names a node the cluster lacks", classNodes: []string{"node-z"},
			nodes: twoNodes, runOn: "node-a", wantDNAT: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Inputs{
				NodeName:   tt.runOn,
				Nodes:      tt.nodes,
				Namespaces: []*corev1.Namespace{ns("games", nil)},
				Classes:    []*v1alpha1.PortMapClass{class("public", tt.classNodes)},
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
			Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a"})},
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
