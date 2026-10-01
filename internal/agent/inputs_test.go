package agent

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
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

	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{}}

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
// buildInputs reads the interfaces the classes give this node off the host
// once per name, and an interface the host cannot resolve is absent so Compute
// turns it into a not-ready row message rather than a skipped row.
func TestBuildInputsResolvesInterfaces(t *testing.T) {
	scheme := testScheme(t)
	classWith := func(name string, ifaces ...string) *v1alpha1.PortMapClass {
		return &v1alpha1.PortMapClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.PortMapClassSpec{
				Nodes: map[string]v1alpha1.NodeInterfaces{"a": {Interfaces: ifaces}},
			},
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

// TestBuildInputsReadsIPv6 hands Compute the two host facts IPv6 delivery
// depends on: each class interface's IPv6 address, and whether the host
// forwards IPv6 at all.
func TestBuildInputsReadsIPv6(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pub"},
		Spec: v1alpha1.PortMapClassSpec{
			Nodes: map[string]v1alpha1.NodeInterfaces{"a": {Interfaces: []string{"eth0", "wt0"}}},
		},
	}).Build()

	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Log: logr.Discard(), Host: &fakeHost{
		ifaceAddr:  map[string]string{"eth0": "192.0.2.10", "wt0": "100.64.0.7"},
		ifaceAddr6: map[string]string{"wt0": "fd7a:115c:a1e0::7"},
		ipv6Fwd:    true,
	}}
	in, err := r.buildInputs(context.Background())
	if err != nil {
		t.Fatalf("buildInputs: %v", err)
	}
	if !in.IPv6Forwarding {
		t.Error("IPv6Forwarding = false, want the host's true")
	}
	if want := map[string]string{"wt0": "fd7a:115c:a1e0::7"}; !reflect.DeepEqual(in.InterfaceAddrs6, want) {
		t.Errorf("InterfaceAddrs6 = %v, want %v", in.InterfaceAddrs6, want)
	}
}

// TestIPv6ForwardingIsLogged: a node with IPv6 forwarding off delivers IPv4 and
// not IPv6, which no PortMap condition shows when the node is not a mapping's
// serving node. The agent says so in its log once when it finds forwarding
// off, again when it comes back, and not on every pass in between. An
// unreadable sysctl counts as off.
func TestIPv6ForwardingIsLogged(t *testing.T) {
	var lines []string
	log := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	host := &fakeHost{ipv6Fwd: false}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Log: log, Host: host}

	pass := func() {
		t.Helper()
		if _, err := r.buildInputs(context.Background()); err != nil {
			t.Fatalf("buildInputs: %v", err)
		}
	}
	count := func(sub string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}

	pass()
	pass()
	if n := count("IPv6 forwarding is off"); n != 1 {
		t.Errorf("%d lines saying forwarding is off after two passes, want 1: %v", n, lines)
	}
	host.ipv6Fwd = true
	pass()
	if n := count("IPv6 forwarding is on"); n != 1 {
		t.Errorf("%d lines saying forwarding is on, want 1: %v", n, lines)
	}
	host.ipv6Fwd, host.ipv6FwdErr = true, os.ErrNotExist
	pass()
	if n := count("IPv6 forwarding is off"); n != 2 {
		t.Errorf("an unreadable sysctl logged %d off lines in all, want 2: %v", n, lines)
	}
}
