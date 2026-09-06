package reconcile

import (
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

func TestStatusOwnedByEndpointHolder(t *testing.T) {
	// node-a accepts, the pod is on node-b. Only node-b, the endpoint holder,
	// writes the mapping's status.
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

	if hasStatus(Compute(world("node-a")), "games", "a") {
		t.Error("node-a is not the endpoint holder and must not write status")
	}
	res := Compute(world("node-b"))
	if !hasStatus(res, "games", "a") {
		t.Fatal("node-b holds the endpoint and must write status")
	}
	st := res.PortMapStatus[types.NamespacedName{Namespace: "games", Name: "a"}]
	if st.Endpoint == nil || st.Endpoint.Node != "node-b" || st.Endpoint.Address != "10.244.5.5" {
		t.Errorf("endpoint = %+v, want node-b/10.244.5.5", st.Endpoint)
	}
}

func TestPublishedRows(t *testing.T) {
	// Two accepting nodes, two interfaces: one Published row per node per
	// interface. The pod is remote, so the endpoint holder node-c writes status.
	in := Inputs{
		NodeName: "node-c",
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
