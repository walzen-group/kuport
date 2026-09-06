package agent

import (
	"context"
	"fmt"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

func vxlanClass(name string, port int32) *v1alpha1.PortMapClass {
	c := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.PortMapClassSpec{ReturnPath: v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan}},
	}
	if port != 0 {
		c.Spec.ReturnPath.Vxlan = &v1alpha1.VxlanConfig{Port: port}
	}
	return c
}

// TestWritePortMapStatus writes an owned PortMap's status and confirms the
// published addresses are filled — a row naming this node from the host, a row
// naming another accepting node from that node's reported class row — while the
// endpoint and conditions come straight from Compute.
func TestWritePortMapStatus(t *testing.T) {
	scheme := testScheme(t)
	cls := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Status: v1alpha1.PortMapClassStatus{Nodes: []v1alpha1.NodeStatus{
			{Name: "b", Ready: true, Addresses: map[string]string{"eth0": "203.0.113.9"}},
		}},
	}
	pm := &v1alpha1.PortMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web", Generation: 3},
		Spec:       v1alpha1.PortMapSpec{ClassName: "public"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pm, cls).WithStatusSubresource(pm, cls).Build()

	r := &Reconciler{
		Client:   c,
		NodeName: "a",
		Now:      fixedNow,
		Host:     &fakeHost{ifaceAddr: map[string]string{"eth0": "192.0.2.10"}},
	}

	desired := v1alpha1.PortMapStatus{
		Endpoint:           &v1alpha1.Endpoint{Node: "a", Address: "10.1.0.5"},
		ObservedGeneration: 3,
		Published: []v1alpha1.PublishedAddress{
			{Node: "a", Interface: "eth0"}, // this node: address from the host
			{Node: "b", Interface: "eth0"}, // remote node: address from its class row
		},
		Conditions: []metav1.Condition{{
			Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionTrue,
			Reason: v1alpha1.ReasonValid, LastTransitionTime: fixedNow(),
		}},
	}

	key := types.NamespacedName{Namespace: "team", Name: "web"}
	if err := r.writePortMap(context.Background(), key, desired); err != nil {
		t.Fatalf("writePortMap: %v", err)
	}

	var got v1alpha1.PortMap
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Endpoint == nil || got.Status.Endpoint.Address != "10.1.0.5" {
		t.Errorf("endpoint = %v, want 10.1.0.5", got.Status.Endpoint)
	}
	if len(got.Status.Published) != 2 {
		t.Fatalf("published rows = %d, want 2", len(got.Status.Published))
	}
	if a := published(got.Status.Published, "a", "eth0"); a != "192.0.2.10" {
		t.Errorf("local published address = %q, want 192.0.2.10", a)
	}
	if b := published(got.Status.Published, "b", "eth0"); b != "203.0.113.9" {
		t.Errorf("remote published address = %q, want 203.0.113.9 from node b's class row", b)
	}
	if meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAccepted) == nil {
		t.Error("Accepted condition not written")
	}
}

// TestWritePortMapStatusPeerNotReported writes a PortMap whose class exists but
// whose peer accepting node has not reported a row yet: the remote published
// row keeps its blank address instead of failing the write, and fills on the
// pass after the peer reports.
func TestWritePortMapStatusPeerNotReported(t *testing.T) {
	scheme := testScheme(t)
	cls := &v1alpha1.PortMapClass{ObjectMeta: metav1.ObjectMeta{Name: "public"}}
	pm := &v1alpha1.PortMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"},
		Spec:       v1alpha1.PortMapSpec{ClassName: "public"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pm, cls).WithStatusSubresource(pm, cls).Build()

	r := &Reconciler{Client: c, NodeName: "a", Host: &fakeHost{}}
	desired := v1alpha1.PortMapStatus{
		Endpoint: &v1alpha1.Endpoint{Node: "a", Address: "10.1.0.5"},
		Published: []v1alpha1.PublishedAddress{
			{Node: "a", Interface: "eth0"}, // unresolvable on this host: stays blank
			{Node: "b", Interface: "eth0"}, // peer has not reported: stays blank
		},
	}

	key := types.NamespacedName{Namespace: "team", Name: "web"}
	if err := r.writePortMap(context.Background(), key, desired); err != nil {
		t.Fatalf("writePortMap: %v", err)
	}
	var got v1alpha1.PortMap
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Published) != 2 {
		t.Fatalf("published rows = %d, want 2", len(got.Status.Published))
	}
	if a := published(got.Status.Published, "a", "eth0"); a != "" {
		t.Errorf("unresolvable local published address = %q, want empty", a)
	}
	if b := published(got.Status.Published, "b", "eth0"); b != "" {
		t.Errorf("unreported peer published address = %q, want empty", b)
	}
}

// TestWriteClassStatusPublishesAddresses confirms the class row this node
// writes carries the resolved addresses of the class's interfaces, which is
// the report the PortMap status writer reads for rows naming this node.
func TestWriteClassStatusPublishesAddresses(t *testing.T) {
	scheme := testScheme(t)
	cls := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Spec: v1alpha1.PortMapClassSpec{
			ReturnPath: v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan},
			Interfaces: []string{"enp1s0", "wt0", "missing0"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cls).WithStatusSubresource(cls).Build()

	r := &Reconciler{
		Client:   c,
		NodeName: "edge-a",
		Now:      fixedNow,
		Host: &fakeHost{ifaceAddr: map[string]string{
			"enp1s0": "203.0.113.9", "wt0": "100.64.93.143",
		}},
	}
	contrib := kreconcile.ClassContribution{Node: v1alpha1.NodeStatus{Name: "edge-a", Ready: true}}

	if err := r.writeClass(context.Background(), "public", contrib, hostState{underlayMTU: 1400, linkMTU: 1350}); err != nil {
		t.Fatalf("writeClass: %v", err)
	}
	var got v1alpha1.PortMapClass
	if err := c.Get(context.Background(), types.NamespacedName{Name: "public"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	row := findNode(got.Status.Nodes, "edge-a")
	if row == nil {
		t.Fatal("own node row missing")
	}
	if row.Addresses["enp1s0"] != "203.0.113.9" || row.Addresses["wt0"] != "100.64.93.143" {
		t.Errorf("row addresses = %v, want enp1s0 203.0.113.9 and wt0 100.64.93.143", row.Addresses)
	}
	if _, ok := row.Addresses["missing0"]; ok {
		t.Errorf("row addresses carry an interface with no resolvable address: %v", row.Addresses)
	}
}

func published(rows []v1alpha1.PublishedAddress, node, iface string) string {
	for _, r := range rows {
		if r.Node == node && r.Interface == iface {
			return r.Address
		}
	}
	return "<absent>"
}

// TestWriteClassStatusConflictRetryRecomputes is the load-bearing test. It
// forces a real 409 on the first status update and, in the same moment, has a
// second agent write a different node's row into the object. The retry must
// re-read that fresh object and merge this node's row onto it, preserving the
// other agent's row. A retry that replayed the stale update would drop the
// other row, and this test would fail — which is the point.
func TestWriteClassStatusConflictRetryRecomputes(t *testing.T) {
	scheme := testScheme(t)
	cls := vxlanClass("public", 0)
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cls).WithStatusSubresource(cls).Build()

	var updates int
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			updates++
			if updates == 1 {
				// A concurrent agent lands node "b"'s row and bumps the object.
				var fresh v1alpha1.PortMapClass
				if err := cl.Get(ctx, types.NamespacedName{Name: "public"}, &fresh); err != nil {
					return err
				}
				fresh.Status.Nodes = append(fresh.Status.Nodes, v1alpha1.NodeStatus{Name: "b", Ready: true})
				if err := cl.Status().Update(ctx, &fresh); err != nil {
					return err
				}
				return kerrors.NewConflict(
					schema.GroupResource{Group: "kuport.dev", Resource: "portmapclasses"},
					obj.GetName(), fmt.Errorf("forced conflict"))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})

	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{}}
	contrib := kreconcile.ClassContribution{Node: v1alpha1.NodeStatus{Name: "a", Ready: true}}

	if err := r.writeClass(context.Background(), "public", contrib, hostState{underlayMTU: 1350, linkMTU: 1300}); err != nil {
		t.Fatalf("writeClass: %v", err)
	}
	if updates < 2 {
		t.Fatalf("status update attempts = %d, want the retry to fire (>=2)", updates)
	}

	var got v1alpha1.PortMapClass
	if err := base.Get(context.Background(), types.NamespacedName{Name: "public"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !hasNode(got.Status.Nodes, "a") {
		t.Error("this node's row (a) missing after retry")
	}
	if !hasNode(got.Status.Nodes, "b") {
		t.Error("concurrent agent's row (b) was clobbered: the retry replayed a stale update instead of recomputing from the fresh object")
	}
	// This node reads its underlay MTU into its own row.
	if row := findNode(got.Status.Nodes, "a"); row == nil || row.UnderlayMTU != 1350 || row.LinkMTU != 1300 {
		t.Errorf("node row MTU = %+v, want underlay 1350 / link 1300", row)
	}
}

// TestLinkClaimCollision has two agents propose a claim that lands on the same
// slot. The first writes it; the second must find the slot taken by another pair
// and leave it alone rather than overwrite, per the recorded-claim rule.
func TestLinkClaimCollision(t *testing.T) {
	scheme := testScheme(t)
	cls := vxlanClass("public", 0)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cls).WithStatusSubresource(cls).Build()

	claim := func(key string, peers []string, slot int32) kreconcile.ClassContribution {
		return kreconcile.ClassContribution{ClaimLinks: []v1alpha1.LinkAllocation{
			{Key: key, Peers: peers, Subnet: "169.254.77.0/31", Slot: slot},
		}}
	}

	agentA := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{}}
	agentB := &Reconciler{Client: c, NodeName: "c", Now: fixedNow, Host: &fakeHost{}}

	// Agent A claims pair a/b at slot 0.
	if err := agentA.writeClass(context.Background(), "public", claim("a/b", []string{"a", "b"}, 0), hostState{}); err != nil {
		t.Fatalf("agent A writeClass: %v", err)
	}
	// Agent B, computed against the same empty status, also wants slot 0 for a/c.
	if err := agentB.writeClass(context.Background(), "public", claim("a/c", []string{"a", "c"}, 0), hostState{}); err != nil {
		t.Fatalf("agent B writeClass: %v", err)
	}

	var got v1alpha1.PortMapClass
	if err := c.Get(context.Background(), types.NamespacedName{Name: "public"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Links) != 1 {
		t.Fatalf("links = %d, want 1 (B must not overwrite A's slot)", len(got.Status.Links))
	}
	if got.Status.Links[0].Key != "a/b" || got.Status.Links[0].Slot != 0 {
		t.Errorf("surviving claim = %+v, want a/b at slot 0", got.Status.Links[0])
	}

	// On its next pass B recomputes against the fresh status and takes slot 1.
	if err := agentB.writeClass(context.Background(), "public", claim("a/c", []string{"a", "c"}, 1), hostState{}); err != nil {
		t.Fatalf("agent B recompute: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "public"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Links) != 2 {
		t.Fatalf("links = %d, want 2 after B's recompute", len(got.Status.Links))
	}
}

// TestConditionOverlay checks the host-only class conditions layer over Compute's
// Ready condition with the right precedence: tunnel mode first, then a port
// collision, and neither for a None-mode class.
func TestConditionOverlay(t *testing.T) {
	validReady := []metav1.Condition{{
		Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonValid,
	}}

	t.Run("native routing refuses the class", func(t *testing.T) {
		got := overlayConditions(validReady, vxlanClass("public", 0),
			hostState{tunnelKnown: true, tunnelOK: false, cniPort: 8472}, fixedNow())
		assertReady(t, got, metav1.ConditionFalse, v1alpha1.ReasonTunnelModeRequired)
	})

	t.Run("port collision refuses the class", func(t *testing.T) {
		got := overlayConditions(validReady, vxlanClass("public", 8472),
			hostState{tunnelKnown: true, tunnelOK: true, cniPort: 8472}, fixedNow())
		assertReady(t, got, metav1.ConditionFalse, v1alpha1.ReasonVxlanPortConflict)
	})

	t.Run("tunnel failure outranks port collision", func(t *testing.T) {
		got := overlayConditions(validReady, vxlanClass("public", 8472),
			hostState{tunnelKnown: true, tunnelOK: false, cniPort: 8472}, fixedNow())
		assertReady(t, got, metav1.ConditionFalse, v1alpha1.ReasonTunnelModeRequired)
	})

	t.Run("tunnel unknown leaves Compute's condition alone", func(t *testing.T) {
		got := overlayConditions(validReady, vxlanClass("public", 4790),
			hostState{tunnelKnown: false, cniPort: 8472}, fixedNow())
		assertReady(t, got, metav1.ConditionTrue, v1alpha1.ReasonValid)
	})

	t.Run("None-mode class is never refused on tunnel grounds", func(t *testing.T) {
		none := &v1alpha1.PortMapClass{
			ObjectMeta: metav1.ObjectMeta{Name: "internal"},
			Spec:       v1alpha1.PortMapClassSpec{ReturnPath: v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathNone}},
		}
		got := overlayConditions(validReady, none, hostState{tunnelKnown: true, tunnelOK: false, cniPort: 8472}, fixedNow())
		assertReady(t, got, metav1.ConditionTrue, v1alpha1.ReasonValid)
	})
}

func assertReady(t *testing.T, conds []metav1.Condition, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(conds, v1alpha1.ConditionReady)
	if c == nil {
		t.Fatal("no Ready condition")
	}
	if c.Status != status || c.Reason != reason {
		t.Errorf("Ready = (%s, %s), want (%s, %s)", c.Status, c.Reason, status, reason)
	}
}

func hasNode(rows []v1alpha1.NodeStatus, name string) bool { return findNode(rows, name) != nil }

func findNode(rows []v1alpha1.NodeStatus, name string) *v1alpha1.NodeStatus {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}
