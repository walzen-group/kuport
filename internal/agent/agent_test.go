package agent

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// TestReconcileEndToEnd runs one whole-node pass over a populated cache with
// faked host and datapath: the datapath is applied, the class node row is
// written with the host's MTU, and the agent reports ready.
func TestReconcileEndToEnd(t *testing.T) {
	scheme := testScheme(t)
	cls := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Spec: v1alpha1.PortMapClassSpec{
			NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "edge"}},
			Interfaces:   []string{"eth0"},
			ReturnPath:   v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node("a", "10.0.0.1", map[string]string{"role": "edge"}), cls).
		WithStatusSubresource(cls).
		Build()

	dp := &fakeDatapath{}
	r := &Reconciler{
		Client:   c,
		NodeName: "a",
		DP:       dp,
		Host: &fakeHost{tunnelOK: true, tunnelKnown: true, underlay: 1350,
			ifaceAddr: map[string]string{"eth0": "192.0.2.10"}},
		Now: fixedNow,
		Log: logr.Discard(),
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(dp.applied) != 1 {
		t.Errorf("Apply called %d times, want 1", len(dp.applied))
	}
	if !r.Ready() {
		t.Error("agent not ready after a completed reconcile")
	}

	var got v1alpha1.PortMapClass
	if err := c.Get(context.Background(), types.NamespacedName{Name: "public"}, &got); err != nil {
		t.Fatalf("get class: %v", err)
	}
	row := findNode(got.Status.Nodes, "a")
	if row == nil {
		t.Fatal("node row for a not written")
	}
	if row.UnderlayMTU != 1350 || row.LinkMTU != 1300 {
		t.Errorf("node row MTU = underlay %d / link %d, want 1350 / 1300", row.UnderlayMTU, row.LinkMTU)
	}
	// The row's readiness is Compute's host self-check: eth0 resolved on the
	// fake host, so the row is ready.
	if !row.Ready || row.Message != "" {
		t.Errorf("node row = %+v, want ready with no message", row)
	}
}
