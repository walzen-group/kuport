package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

// TestPodChangedIgnoresFieldsTheReconcileSkips: every pod in the cluster is
// watched, so an update that moves only fields the reconcile never reads,
// a container status or a resource version, enqueues nothing.
func TestPodChangedIgnoresFieldsTheReconcileSkips(t *testing.T) {
	base := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "qb-0", Namespace: "media", ResourceVersion: "1", Labels: map[string]string{"app": "qb"}},
		Spec:       corev1.PodSpec{NodeName: "node-b"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.244.5.5"}}},
	}
	noise := base.DeepCopy()
	noise.ResourceVersion = "2"
	noise.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "qb", RestartCount: 3}}
	if podChanged.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: noise}) {
		t.Error("an update to a container status enqueued a pass")
	}

	moved := base.DeepCopy()
	moved.Status.PodIPs = []corev1.PodIP{{IP: "10.244.5.9"}}
	if !podChanged.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: moved}) {
		t.Error("a new pod address enqueued no pass")
	}
	relabelled := base.DeepCopy()
	relabelled.Labels = map[string]string{"app": "other"}
	if !podChanged.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: relabelled}) {
		t.Error("a label change enqueued no pass")
	}
}

// TestPodFieldsKeepsWhatTheReconcileReads: the cache transform drops the pod's
// spec and status down to node, phase and addresses, and keeps its labels.
func TestPodFieldsKeepsWhatTheReconcileReads(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "qb-0", Namespace: "media", Labels: map[string]string{"app": "qb"}, Annotations: map[string]string{"big": "x"}},
		Spec:       corev1.PodSpec{NodeName: "node-b", Containers: []corev1.Container{{Name: "qb", Image: "qbittorrent"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIPs: []corev1.PodIP{{IP: "10.244.5.5"}}},
	}
	got := PodFields(p)
	if got.Labels["app"] != "qb" || got.Spec.NodeName != "node-b" || got.Status.PodIPs[0].IP != "10.244.5.5" || got.Status.Phase != corev1.PodRunning {
		t.Errorf("PodFields dropped a field the reconcile reads: %+v", got)
	}
	if len(got.Spec.Containers) != 0 || len(got.Annotations) != 0 {
		t.Errorf("PodFields kept containers %v and annotations %v", got.Spec.Containers, got.Annotations)
	}
}

// TestWriteEgressStatus writes an owned Egress's status and leaves the spec.
func TestWriteEgressStatus(t *testing.T) {
	eg := &v1alpha1.Egress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "qb"},
		Spec:       v1alpha1.EgressSpec{ClassName: "out", PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "qb"}}, FailClosed: true},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(eg).WithStatusSubresource(eg).Build()
	r := &Reconciler{Client: c, NodeName: "node-a"}
	want := v1alpha1.EgressStatus{
		Node: "node-a",
		Pods: []v1alpha1.EgressPod{{Name: "qb-0", Node: "node-b", Address: "10.244.5.5", Path: v1alpha1.EgressPathLink}},
		Conditions: []metav1.Condition{{
			Type: v1alpha1.ConditionProgrammed, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonAllPodsRouted,
			LastTransitionTime: fixedNow(),
		}},
	}
	key := types.NamespacedName{Namespace: "media", Name: "qb"}
	if err := r.writeEgress(context.Background(), key, want); err != nil {
		t.Fatalf("writeEgress: %v", err)
	}
	var got v1alpha1.Egress
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Node != "node-a" || len(got.Status.Pods) != 1 || got.Status.Pods[0].Path != v1alpha1.EgressPathLink {
		t.Errorf("status = %+v, want node-a with qb-0 on Link", got.Status)
	}
	if !got.Spec.FailClosed {
		t.Error("the status write changed the spec")
	}
}

// TestWriteEgressClassRow writes this node's row with its MTU figures and keeps
// another node's row.
func TestWriteEgressClassRow(t *testing.T) {
	cls := &v1alpha1.EgressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "out"},
		Spec:       v1alpha1.EgressClassSpec{Node: "node-a"},
		Status:     v1alpha1.EgressClassStatus{Nodes: []v1alpha1.NodeStatus{{Name: "node-a", Ready: true}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cls).WithStatusSubresource(cls).Build()
	r := &Reconciler{Client: c, NodeName: "node-b"}
	contrib := kreconcile.EgressClassContribution{Node: v1alpha1.NodeStatus{Name: "node-b", Ready: true}}
	if err := r.writeEgressClass(context.Background(), "out", contrib, hostState{underlayMTU: 1350, linkMTU: 1300}); err != nil {
		t.Fatalf("writeEgressClass: %v", err)
	}
	var got v1alpha1.EgressClass
	if err := c.Get(context.Background(), types.NamespacedName{Name: "out"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !hasNode(got.Status.Nodes, "node-a") {
		t.Error("node-a's row was dropped")
	}
	if row := findNode(got.Status.Nodes, "node-b"); row == nil || row.UnderlayMTU != 1350 || row.LinkMTU != 1300 {
		t.Errorf("node-b row = %+v, want underlay 1350 and link 1300", row)
	}
}
