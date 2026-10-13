package agent

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/walzen-group/kuport/internal/metrics"
)

// TestReconcileSkipsUnchangedState runs passes over the same cluster: the second
// pass computes the state the first applied and leaves the datapath alone, a
// changed host address applies again, and a pass five minutes after the last
// apply applies the unchanged state to repair the host.
func TestReconcileSkipsUnchangedState(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node("a", "10.0.0.1", nil)).Build()
	dp := &fakeDatapath{}
	host := &fakeHost{tunnelOK: true, tunnelKnown: true, underlay: 1350,
		localAddrs: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	now := baseTime
	r := &Reconciler{
		Client: c, NodeName: "a", DP: dp, Host: host,
		Now: func() metav1.Time { return metav1.NewTime(now) }, Log: logr.Discard(),
	}
	ok := testutil.ToFloat64(metrics.ApplyTotal.WithLabelValues("ok"))
	pass := func(at time.Duration) {
		t.Helper()
		now = baseTime.Add(at)
		res, err := r.Reconcile(context.Background(), reconcile.Request{})
		if err != nil {
			t.Fatalf("Reconcile at %v: %v", at, err)
		}
		if res.RequeueAfter != hostPollPeriod {
			t.Fatalf("RequeueAfter %v at %v, want %v", res.RequeueAfter, at, hostPollPeriod)
		}
	}

	pass(0)
	pass(time.Minute)
	if len(dp.applied) != 1 {
		t.Fatalf("an unchanged pass applied: %d applies, want 1", len(dp.applied))
	}

	host.localAddrs = append(host.localAddrs, netip.MustParseAddr("100.64.0.1"))
	pass(2 * time.Minute)
	if len(dp.applied) != 2 {
		t.Fatalf("a changed address left the datapath alone: %d applies, want 2", len(dp.applied))
	}

	pass(6 * time.Minute)
	if len(dp.applied) != 2 {
		t.Fatalf("a pass four minutes after the last apply applied: %d applies, want 2", len(dp.applied))
	}
	pass(7 * time.Minute)
	if len(dp.applied) != 3 {
		t.Fatalf("a pass five minutes after the last apply skipped: %d applies, want 3", len(dp.applied))
	}
	if got := testutil.ToFloat64(metrics.ApplyTotal.WithLabelValues("ok")) - ok; got != 3 {
		t.Errorf("ok applies rose by %v, want 3", got)
	}
}

// TestReconcileRepairHonoursResync applies an unchanged state again once the
// configured Repair period, the --resync flag, has passed.
func TestReconcileRepairHonoursResync(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node("a", "10.0.0.1", nil)).Build()
	dp := &fakeDatapath{}
	now := baseTime
	r := &Reconciler{
		Client: c, NodeName: "a", DP: dp, Repair: 2 * time.Minute,
		Host: &fakeHost{tunnelOK: true, tunnelKnown: true, underlay: 1350},
		Now:  func() metav1.Time { return metav1.NewTime(now) }, Log: logr.Discard(),
	}
	for _, at := range []time.Duration{0, time.Minute, 2 * time.Minute} {
		now = baseTime.Add(at)
		if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
			t.Fatalf("Reconcile at %v: %v", at, err)
		}
	}
	if len(dp.applied) != 2 {
		t.Errorf("%d applies over 0, 1 and 2 minutes with a 2 minute repair period, want 2", len(dp.applied))
	}
}

// TestReconcileRetriesFailedApply applies state A, fails to apply state B, and
// then computes A again. The host may hold half of B, so the third pass applies
// A although A was the last state applied successfully.
func TestReconcileRetriesFailedApply(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node("a", "10.0.0.1", nil)).Build()
	dp := &fakeDatapath{}
	a := []netip.Addr{netip.MustParseAddr("10.0.0.1")}
	b := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("100.64.0.1")}
	host := &fakeHost{tunnelOK: true, tunnelKnown: true, underlay: 1350, localAddrs: a}
	r := &Reconciler{
		Client: c, NodeName: "a", DP: dp, Host: host,
		Now: fixedNow, Log: logr.Discard(),
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile A: %v", err)
	}
	host.localAddrs = b
	dp.applyErr = errors.New("netlink: operation not permitted")
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err == nil {
		t.Fatal("Reconcile B succeeded with a failing datapath")
	}
	host.localAddrs = a
	dp.applyErr = nil
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile A again: %v", err)
	}
	if len(dp.applied) != 3 {
		t.Errorf("%d applies, want 3: A, the failed B, and A again", len(dp.applied))
	}
}

// TestNodeChanged passes a Node update only when a field the reconcile reads
// moved. The kubelet's status report every five minutes moves only heartbeat
// times and the resource version.
func TestNodeChanged(t *testing.T) {
	base := func() *corev1.Node {
		n := node("a", "10.0.0.1", map[string]string{"zone": "x"})
		n.ResourceVersion = "100"
		n.Spec.PodCIDRs = []string{"10.244.1.0/24"}
		n.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.NewTime(baseTime)},
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, LastHeartbeatTime: metav1.NewTime(baseTime)},
		}
		return n
	}
	cases := []struct {
		name   string
		mutate func(*corev1.Node)
		want   bool
	}{
		{"status report", func(n *corev1.Node) {
			n.ResourceVersion = "101"
			for i := range n.Status.Conditions {
				n.Status.Conditions[i].LastHeartbeatTime = metav1.NewTime(baseTime.Add(5 * time.Minute))
			}
		}, false},
		{"memory pressure", func(n *corev1.Node) { n.Status.Conditions[1].Status = corev1.ConditionTrue }, false},
		{"label", func(n *corev1.Node) { n.Labels["zone"] = "y" }, false},
		{"InternalIP", func(n *corev1.Node) { n.Status.Addresses[0].Address = "10.0.0.2" }, true},
		{"added address", func(n *corev1.Node) {
			n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fd00::1"})
		}, true},
		{"pod CIDRs", func(n *corev1.Node) { n.Spec.PodCIDRs = []string{"10.244.2.0/24"} }, true},
		{"not ready", func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionUnknown }, true},
		{"deleting", func(n *corev1.Node) { n.DeletionTimestamp = &metav1.Time{Time: baseTime} }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, next := base(), base()
			c.mutate(next)
			if got := nodeChanged.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: next}); got != c.want {
				t.Errorf("nodeChanged = %v, want %v", got, c.want)
			}
		})
	}
}
