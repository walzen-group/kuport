//go:build envtest

// This file runs only under `-tags envtest`. It stands up a real API server
// through controller-runtime's envtest, with no nodes, and exercises the status
// writing and condition paths against it — the plumbing the fake client cannot
// prove. The default `go test ./...` never compiles this file, so a missing
// envtest binary can never make the ordinary suite silently skip and report
// green; running it is a deliberate, tagged act.
//
//	KUBEBUILDER_ASSETS=$(setup-envtest use -p path) go test -tags envtest ./internal/agent/...
package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

func startEnv(t *testing.T) (client.Client, func()) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run `setup-envtest use -p path` and export it")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Skipf("envtest could not start (assets unavailable): %v", err)
	}
	scheme := testScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("build client: %v", err)
	}
	return c, func() { _ = env.Stop() }
}

// TestEnvtestClassStatus writes a class's node row and a host-overlaid condition
// against a real API server and reads them back through the status subresource.
func TestEnvtestClassStatus(t *testing.T) {
	c, stop := startEnv(t)
	defer stop()
	ctx := context.Background()

	cls := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Spec: v1alpha1.PortMapClassSpec{
			Nodes:      map[string]v1alpha1.NodeInterfaces{"a": {Interfaces: []string{"eth0"}}},
			ReturnPath: v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan},
		},
	}
	if err := c.Create(ctx, cls); err != nil {
		t.Fatalf("create class: %v", err)
	}

	// A port collision is the one host condition that still refuses a class. The
	// CNI's routing mode was a second until v0.4.0, when both legs moved onto
	// kuport's own link and the mode stopped deciding anything, so tunnelOK is
	// false here and no longer changes the verdict.
	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{}}
	contrib := kreconcile.ClassContribution{Node: v1alpha1.NodeStatus{Name: "a", Ready: true}}
	hs := hostState{tunnelKnown: true, tunnelOK: false, cniPort: 4790, linkPort: 4790, underlayMTU: 1350, linkMTU: 1300}
	if err := r.writeClass(ctx, "public", contrib, hs); err != nil {
		t.Fatalf("writeClass: %v", err)
	}

	var got v1alpha1.PortMapClass
	if err := c.Get(ctx, types.NamespacedName{Name: "public"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if findNode(got.Status.Nodes, "a") == nil {
		t.Error("node row a not persisted")
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Reason != v1alpha1.ReasonVxlanPortConflict {
		t.Errorf("Ready condition = %v, want VxlanPortConflict overlay", ready)
	}
}

// TestEnvtestLinkPool proves on a real API server what the pool rests on: a
// second create of a slot's name is refused whichever class asks, a Link's spec
// cannot change, and a deprecated vxlan block is stored as written with no
// defaults filled in.
func TestEnvtestLinkPool(t *testing.T) {
	c, stop := startEnv(t)
	defer stop()
	ctx := context.Background()

	link := func(className string, peers ...string) *v1alpha1.Link {
		return &v1alpha1.Link{
			ObjectMeta: metav1.ObjectMeta{Name: "slot-3"},
			Spec:       v1alpha1.LinkSpec{ClassKind: v1alpha1.ClassKindPortMapClass, ClassName: className, Peers: peers},
		}
	}
	if err := c.Create(ctx, link("public", "a", "b")); err != nil {
		t.Fatalf("create slot-3: %v", err)
	}
	if err := c.Create(ctx, link("private", "a", "c")); !kerrors.IsAlreadyExists(err) {
		t.Errorf("second create of slot-3 = %v, want AlreadyExists", err)
	}

	var got v1alpha1.Link
	if err := c.Get(ctx, types.NamespacedName{Name: "slot-3"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.Peers = []string{"a", "z"}
	if err := c.Update(ctx, &got); err == nil {
		t.Error("changing a Link's peers was accepted, want the spec immutable")
	}

	cls := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy"},
		Spec: v1alpha1.PortMapClassSpec{
			Nodes: map[string]v1alpha1.NodeInterfaces{"a": {Interfaces: []string{"eth0"}}},
			ReturnPath: v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan,
				Vxlan: &v1alpha1.VxlanConfig{Subnet: "169.254.78.0/24"}},
		},
	}
	if err := c.Create(ctx, cls); err != nil {
		t.Fatalf("create class with a deprecated vxlan block: %v", err)
	}
	var stored v1alpha1.PortMapClass
	if err := c.Get(ctx, types.NamespacedName{Name: "legacy"}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if v := stored.Spec.ReturnPath.Vxlan; v.Subnet6 != "" || v.Port != 0 || v.VNI != 0 {
		t.Errorf("vxlan = %+v, want only the subnet written and no defaults filled in", *v)
	}
}

// TestEnvtestPortMapStatus writes a PortMap's status subresource and reads it back.
func TestEnvtestPortMapStatus(t *testing.T) {
	c, stop := startEnv(t)
	defer stop()
	ctx := context.Background()

	if err := c.Create(ctx, &v1alpha1.PortMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec: v1alpha1.PortMapSpec{
			ClassName: "public", Protocol: v1alpha1.ProtocolTCP, Port: 8080,
			ServiceRef: v1alpha1.ServiceRef{Name: "web", Port: "http"},
		},
	}); err != nil {
		t.Fatalf("create portmap: %v", err)
	}

	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{ifaceAddr: map[string]string{"eth0": "192.0.2.10"}}}
	desired := v1alpha1.PortMapStatus{
		Endpoint:   &v1alpha1.Endpoint{Node: "a", Address: "10.1.0.5"},
		Published:  []v1alpha1.PublishedAddress{{Node: "a", Interface: "eth0"}},
		Conditions: []metav1.Condition{{Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonValid, LastTransitionTime: fixedNow()}},
	}
	key := types.NamespacedName{Namespace: "default", Name: "web"}
	if err := r.writePortMap(ctx, key, desired); err != nil {
		t.Fatalf("writePortMap: %v", err)
	}

	var got v1alpha1.PortMap
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Endpoint == nil || got.Status.Endpoint.Node != "a" {
		t.Errorf("endpoint not persisted: %v", got.Status.Endpoint)
	}
	if published(got.Status.Published, "a", "eth0") != "192.0.2.10" {
		t.Errorf("published address not filled: %v", got.Status.Published)
	}
}
