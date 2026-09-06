package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// TestBuildInputs asserts the reconciler reads every watched kind out of the
// cache into the snapshot Compute consumes, and stamps it with this node's name
// and the injected clock.
func TestBuildInputs(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node("a", "10.0.0.1", map[string]string{"role": "edge"}),
		node("b", "10.0.0.2", nil),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}},
		&v1alpha1.PortMapClass{ObjectMeta: metav1.ObjectMeta{Name: "public"}},
		&v1alpha1.PortMap{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"}},
		&discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Namespace: "team", Name: "web-xyz"},
			AddressType: discoveryv1.AddressTypeIPv4,
		},
	).Build()

	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow}

	in, err := r.buildInputs(context.Background())
	if err != nil {
		t.Fatalf("buildInputs: %v", err)
	}

	if in.NodeName != "a" {
		t.Errorf("NodeName = %q, want a", in.NodeName)
	}
	if !in.Now.Equal(&metav1.Time{Time: baseTime}) {
		t.Errorf("Now = %v, want injected %v", in.Now, baseTime)
	}
	if len(in.Nodes) != 2 {
		t.Errorf("Nodes = %d, want 2", len(in.Nodes))
	}
	if len(in.Namespaces) != 1 {
		t.Errorf("Namespaces = %d, want 1", len(in.Namespaces))
	}
	if len(in.Classes) != 1 {
		t.Errorf("Classes = %d, want 1", len(in.Classes))
	}
	if len(in.PortMaps) != 1 {
		t.Errorf("PortMaps = %d, want 1", len(in.PortMaps))
	}
	if len(in.Slices) != 1 {
		t.Errorf("Slices = %d, want 1", len(in.Slices))
	}
}

// TestBuildInputsResolvesInterfaces covers the F2 host self-check seam:
// buildInputs reads the union of the classes' interfaces off the host once
// per name, and an interface the host cannot resolve is absent so Compute
// turns it into a not-ready row message rather than a skipped row.
func TestBuildInputsResolvesInterfaces(t *testing.T) {
	scheme := testScheme(t)
	classWith := func(name string, ifaces ...string) *v1alpha1.PortMapClass {
		return &v1alpha1.PortMapClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       v1alpha1.PortMapClassSpec{Interfaces: ifaces},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		classWith("pub", "eth0", "wt0"),
		classWith("wan", "eth0", "missing0"),
	).Build()

	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{
		ifaceAddr: map[string]string{
			"eth0": "192.0.2.10", "wt0": "100.64.0.7",
		},
	}}

	in, err := r.buildInputs(context.Background())
	if err != nil {
		t.Fatalf("buildInputs: %v", err)
	}
	if in.InterfaceAddrs["eth0"] != "192.0.2.10" || in.InterfaceAddrs["wt0"] != "100.64.0.7" {
		t.Errorf("resolved = %v, want eth0 and wt0", in.InterfaceAddrs)
	}
	if _, ok := in.InterfaceAddrs["missing0"]; ok {
		t.Errorf("unresolvable interface made it into the map: %v", in.InterfaceAddrs)
	}
}
