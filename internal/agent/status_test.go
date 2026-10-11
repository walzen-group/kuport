package agent

import (
	"context"
	"fmt"
	"reflect"
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

func vxlanClass(name string) *v1alpha1.PortMapClass {
	return &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.PortMapClassSpec{ReturnPath: v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan}},
	}
}

func poolLink(slot int, peers ...string) *v1alpha1.Link {
	return &v1alpha1.Link{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("slot-%d", slot)},
		Spec: v1alpha1.LinkSpec{
			ClassKind: v1alpha1.ClassKindPortMapClass,
			ClassName: "public",
			Peers:     peers,
		},
	}
}

// TestWriteLinksCreateLosesTheRace: two agents computed against the same pool
// both create slot-1. The second create finds the name taken, which is how the
// API server keeps two links off one slot; the agent leaves the first Link as
// it is and reports no error, and its next pass picks another slot.
func TestWriteLinksCreateLosesTheRace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.Link{}).Build()
	ctx := context.Background()

	first := &Reconciler{Client: c, NodeName: "node-b"}
	second := &Reconciler{Client: c, NodeName: "node-c"}
	if err := first.writeLinks(ctx, kreconcile.LinkActions{Create: []v1alpha1.Link{*poolLink(1, "node-a", "node-b")}}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := second.writeLinks(ctx, kreconcile.LinkActions{Create: []v1alpha1.Link{*poolLink(1, "node-a", "node-c")}}); err != nil {
		t.Fatalf("second create returned %v, want AlreadyExists swallowed", err)
	}

	var got v1alpha1.Link
	if err := c.Get(ctx, types.NamespacedName{Name: "slot-1"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Peers[1] != "node-b" {
		t.Errorf("slot-1 peers = %v, want the first claim's [node-a node-b] kept", got.Spec.Peers)
	}
}

// TestWriteLinksPersistsUnusedSince is the release regression. Before the Link
// kind, the agent passed the computed UnusedSince through a merge that cleared
// it on every existing claim, so no claim was ever released. The value Compute
// decides is now the value written.
func TestWriteLinksPersistsUnusedSince(t *testing.T) {
	l := poolLink(0, "node-a", "node-b")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(l).WithStatusSubresource(&v1alpha1.Link{}).Build()
	ctx := context.Background()

	var seen v1alpha1.Link
	if err := c.Get(ctx, types.NamespacedName{Name: "slot-0"}, &seen); err != nil {
		t.Fatalf("get: %v", err)
	}
	now := fixedNow()
	r := &Reconciler{Client: c, NodeName: "node-c"}
	if err := r.writeLinks(ctx, kreconcile.LinkActions{Status: []kreconcile.LinkStatusUpdate{
		{Name: "slot-0", ResourceVersion: seen.ResourceVersion, UnusedSince: &now},
	}}); err != nil {
		t.Fatalf("writeLinks: %v", err)
	}

	var got v1alpha1.Link
	if err := c.Get(ctx, types.NamespacedName{Name: "slot-0"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.UnusedSince == nil || !got.Status.UnusedSince.Equal(&now) {
		t.Errorf("UnusedSince = %v, want %v written", got.Status.UnusedSince, now)
	}
}

// TestWriteLinksSkipsAMovedObject: an update computed against an older version
// of the Link is dropped. The newer version may carry a decision this pass
// never saw, such as another agent clearing UnusedSince because the pair is
// needed again.
func TestWriteLinksSkipsAMovedObject(t *testing.T) {
	l := poolLink(0, "node-a", "node-b")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(l).WithStatusSubresource(&v1alpha1.Link{}).Build()
	ctx := context.Background()

	now := fixedNow()
	r := &Reconciler{Client: c, NodeName: "node-c"}
	if err := r.writeLinks(ctx, kreconcile.LinkActions{Status: []kreconcile.LinkStatusUpdate{
		{Name: "slot-0", ResourceVersion: "1", UnusedSince: &now},
	}}); err != nil {
		t.Fatalf("writeLinks: %v", err)
	}
	var got v1alpha1.Link
	if err := c.Get(ctx, types.NamespacedName{Name: "slot-0"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ResourceVersion == "1" {
		t.Fatal("fixture: the object must be past version 1 for this test to mean anything")
	}
	if got.Status.UnusedSince != nil {
		t.Errorf("UnusedSince = %v, want nothing written against a moved object", got.Status.UnusedSince)
	}
}

// TestWriteLinksDeleteHonoursTheVersion: a delete goes through against the
// version Compute saw and is refused against any other.
func TestWriteLinksDeleteHonoursTheVersion(t *testing.T) {
	l := poolLink(0, "node-a", "node-b")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(l).WithStatusSubresource(&v1alpha1.Link{}).Build()
	ctx := context.Background()
	r := &Reconciler{Client: c, NodeName: "node-c"}

	if err := r.writeLinks(ctx, kreconcile.LinkActions{Delete: []kreconcile.LinkDelete{{Name: "slot-0", ResourceVersion: "1"}}}); err != nil {
		t.Fatalf("stale delete: %v", err)
	}
	var got v1alpha1.Link
	if err := c.Get(ctx, types.NamespacedName{Name: "slot-0"}, &got); err != nil {
		t.Fatalf("the delete against an old version removed the Link: %v", err)
	}

	if err := r.writeLinks(ctx, kreconcile.LinkActions{Delete: []kreconcile.LinkDelete{{Name: "slot-0", ResourceVersion: got.ResourceVersion}}}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "slot-0"}, &got); !kerrors.IsNotFound(err) {
		t.Errorf("get after delete = %v, want NotFound", err)
	}
}

// TestWriteClassClearsLegacyLinks: the accepting agent removes the status.links
// a release before the Link kind wrote, and keeps every other field.
func TestWriteClassClearsLegacyLinks(t *testing.T) {
	cls := vxlanClass("public")
	cls.Status.Links = []v1alpha1.LinkAllocation{{Key: "node-a/node-b", Peers: []string{"node-a", "node-b"}, Subnet: "169.254.77.0/31"}}
	cls.Status.Nodes = []v1alpha1.NodeStatus{{Name: "b", Ready: true}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(cls).WithStatusSubresource(cls).Build()
	ctx := context.Background()

	r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{}}
	contrib := kreconcile.ClassContribution{Node: v1alpha1.NodeStatus{Name: "a", Ready: true}, ClearLegacyLinks: true}
	if err := r.writeClass(ctx, "public", contrib, hostState{}); err != nil {
		t.Fatalf("writeClass: %v", err)
	}
	var got v1alpha1.PortMapClass
	if err := c.Get(ctx, types.NamespacedName{Name: "public"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Links) != 0 {
		t.Errorf("status.links = %+v, want it removed", got.Status.Links)
	}
	if !hasNode(got.Status.Nodes, "b") {
		t.Error("another agent's row b was removed with the legacy links")
	}
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

// TestWritePortMapStatusFillsIPv6: when the mapping is delivered over IPv6,
// which Compute signals with the endpoint's IPv6 address, each published row
// also carries its interface's IPv6 address, read off the host for this node
// and from the reported class row for another. When it is not, no row carries
// one, even where the interface has an IPv6 address: that address would not
// answer.
func TestWritePortMapStatusFillsIPv6(t *testing.T) {
	cls := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Status: v1alpha1.PortMapClassStatus{Nodes: []v1alpha1.NodeStatus{{
			Name: "b", Ready: true,
			Addresses:  map[string]string{"wt0": "100.64.0.9"},
			Addresses6: map[string]string{"wt0": "fd7a:115c:a1e0::9"},
		}}},
	}
	write := func(endpoint6 string) []v1alpha1.PublishedAddress {
		t.Helper()
		pm := &v1alpha1.PortMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"},
			Spec:       v1alpha1.PortMapSpec{ClassName: "public"},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(pm, cls.DeepCopy()).WithStatusSubresource(pm, cls).Build()
		r := &Reconciler{Client: c, NodeName: "a", Now: fixedNow, Host: &fakeHost{
			ifaceAddr:  map[string]string{"wt0": "100.64.0.7"},
			ifaceAddr6: map[string]string{"wt0": "fd7a:115c:a1e0::7"},
		}}
		key := types.NamespacedName{Namespace: "team", Name: "web"}
		desired := v1alpha1.PortMapStatus{
			Endpoint:  &v1alpha1.Endpoint{Node: "a", Address: "10.1.0.5", Address6: endpoint6},
			Published: []v1alpha1.PublishedAddress{{Node: "a", Interface: "wt0"}, {Node: "b", Interface: "wt0"}},
		}
		if err := r.writePortMap(context.Background(), key, desired); err != nil {
			t.Fatalf("writePortMap: %v", err)
		}
		var got v1alpha1.PortMap
		if err := c.Get(context.Background(), key, &got); err != nil {
			t.Fatalf("get: %v", err)
		}
		return got.Status.Published
	}

	rows := write("fd00:10:1::5")
	want := []v1alpha1.PublishedAddress{
		{Node: "a", Interface: "wt0", Address: "100.64.0.7", Address6: "fd7a:115c:a1e0::7"},
		{Node: "b", Interface: "wt0", Address: "100.64.0.9", Address6: "fd7a:115c:a1e0::9"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("published = %+v, want %+v", rows, want)
	}

	for _, r := range write("") {
		if r.Address6 != "" {
			t.Errorf("published row %+v carries an ipv6 address for a mapping not delivered over ipv6", r)
		}
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
// writes carries what Compute built: the readiness verdict, the message
// naming the interface that did not resolve on the host, and the addresses
// of the interfaces that did. writeClass stamps only the MTU numbers, so an
// unresolvable interface surfaces as a message, never as a dropped row or a
// silently missing address.
func TestWriteClassStatusPublishesAddresses(t *testing.T) {
	scheme := testScheme(t)
	cls := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Spec: v1alpha1.PortMapClassSpec{
			ReturnPath: v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan},
			Nodes: map[string]v1alpha1.NodeInterfaces{
				"edge-a": {Interfaces: []string{"enp1s0", "wt0", "missing0"}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cls).WithStatusSubresource(cls).Build()

	r := &Reconciler{
		Client:   c,
		NodeName: "edge-a",
		Now:      fixedNow,
		Host:     &fakeHost{},
	}
	contrib := kreconcile.ClassContribution{Node: v1alpha1.NodeStatus{
		Name:    "edge-a",
		Ready:   false,
		Message: "interface missing0 not present",
		Addresses: map[string]string{
			"enp1s0": "203.0.113.9", "wt0": "100.64.93.143",
		},
	}}

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
	if row.Ready || row.Message != "interface missing0 not present" {
		t.Errorf("row = %+v, want not-ready with Compute's message intact", row)
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
	cls := vxlanClass("public")
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
					schema.GroupResource{Group: "kuport.wlz.li", Resource: "portmapclasses"},
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

// TestConditionOverlay checks the host-only class conditions layer over
// Compute's Ready condition: a port collision refuses the class, and a
// None-mode class takes neither.
//
// The CNI's routing mode was checked here until v0.4.0, when both legs moved
// onto kuport's own return link and the mode stopped deciding anything. A class
// is accepted now whether the CNI tunnels or routes natively.
func TestConditionOverlay(t *testing.T) {
	validReady := []metav1.Condition{{
		Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonValid,
	}}

	t.Run("native routing no longer refuses the class", func(t *testing.T) {
		got := overlayConditions(validReady, vxlanClass("public"),
			hostState{tunnelKnown: true, tunnelOK: false, cniPort: 8472, linkPort: 4790}, fixedNow())
		assertReady(t, got, metav1.ConditionTrue, v1alpha1.ReasonValid)
	})

	t.Run("a link port on the CNI's refuses the class", func(t *testing.T) {
		got := overlayConditions(validReady, vxlanClass("public"),
			hostState{tunnelKnown: true, tunnelOK: true, cniPort: 8472, linkPort: 8472}, fixedNow())
		assertReady(t, got, metav1.ConditionFalse, v1alpha1.ReasonVxlanPortConflict)
	})

	t.Run("a class's deprecated port is ignored", func(t *testing.T) {
		cls := vxlanClass("public")
		cls.Spec.ReturnPath.Vxlan = &v1alpha1.VxlanConfig{Port: 8472}
		got := overlayConditions(validReady, cls,
			hostState{tunnelKnown: true, tunnelOK: false, cniPort: 8472, linkPort: 4790}, fixedNow())
		assertReady(t, got, metav1.ConditionTrue, v1alpha1.ReasonValid)
	})

	t.Run("tunnel unknown leaves Compute's condition alone", func(t *testing.T) {
		got := overlayConditions(validReady, vxlanClass("public"),
			hostState{tunnelKnown: false, cniPort: 8472, linkPort: 4790}, fixedNow())
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
