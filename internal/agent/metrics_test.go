package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
	"github.com/walzen-group/kuport/internal/metrics"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

// TestReconcileRecordsApply runs one pass and reads the apply series and the
// snapshot the collector serves.
func TestReconcileRecordsApply(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node("a", "10.0.0.1", nil)).Build()
	r := &Reconciler{
		Client: c, NodeName: "a", DP: &fakeDatapath{},
		Host: &fakeHost{tunnelOK: true, tunnelKnown: true, underlay: 1350},
		Now:  fixedNow, Log: logr.Discard(),
	}
	if r.snap.Load() != nil {
		t.Fatal("snapshot set before the first apply")
	}
	ok := testutil.ToFloat64(metrics.ApplyTotal.WithLabelValues("ok"))
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := testutil.ToFloat64(metrics.ApplyTotal.WithLabelValues("ok")) - ok; got != 1 {
		t.Errorf("ok applies rose by %v, want 1", got)
	}
	if testutil.ToFloat64(metrics.LastApplySuccess) == 0 {
		t.Error("last apply success not set")
	}
	if r.snap.Load() == nil {
		t.Error("no snapshot after a successful apply")
	}
}

// TestReconcileCountsFailedApply leaves the snapshot alone and counts the error.
func TestReconcileCountsFailedApply(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node("a", "10.0.0.1", nil)).Build()
	r := &Reconciler{
		Client: c, NodeName: "a", DP: &fakeDatapath{applyErr: errors.New("netlink: operation not permitted")},
		Host: &fakeHost{tunnelOK: true, tunnelKnown: true, underlay: 1350},
		Now:  fixedNow, Log: logr.Discard(),
	}
	errs := testutil.ToFloat64(metrics.ApplyTotal.WithLabelValues("error"))
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err == nil {
		t.Fatal("Reconcile succeeded with a failing datapath")
	}
	if got := testutil.ToFloat64(metrics.ApplyTotal.WithLabelValues("error")) - errs; got != 1 {
		t.Errorf("error applies rose by %v, want 1", got)
	}
	if r.snap.Load() != nil {
		t.Error("snapshot stored for a failed apply")
	}
}

// TestLinkInfos lists only the Links this node peers in and holds a device for.
func TestLinkInfos(t *testing.T) {
	link := func(name, class string, peers ...string) *v1alpha1.Link {
		return &v1alpha1.Link{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       v1alpha1.LinkSpec{ClassKind: "PortMapClass", ClassName: class, Peers: peers},
		}
	}
	in := kreconcile.Inputs{Links: []*v1alpha1.Link{
		link("slot-3", "public", "a", "c"),
		link("slot-0", "overlay", "b", "a"),
		link("slot-7", "public", "b", "c"), // not this node's
		link("slot-9", "public", "a", "d"), // this node's, no device in the State
	}}
	st := datapath.State{Links: []datapath.Link{{Name: "kup-0"}, {Name: "kup-3"}}}
	want := []metrics.LinkInfo{
		{Device: "kup-0", Slot: "0", Class: "overlay", Peer: "b"},
		{Device: "kup-3", Slot: "3", Class: "public", Peer: "c"},
	}
	if got := linkInfos(in, st, "a"); !reflect.DeepEqual(got, want) {
		t.Errorf("link infos:\n got %+v\nwant %+v", got, want)
	}
}
