package reconcile

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// withSlicePortNumber gives every slice's named port a number, the way the
// EndpointSlice controller writes it from the pod's containerPort.
func withSlicePortNumber(in Inputs, num int32) Inputs {
	for _, sl := range in.Slices {
		for i := range sl.Ports {
			sl.Ports[i].Port = ptr(num)
		}
	}
	return in
}

// localWorld is one accepting node holding the pod itself.
func localWorld(runOn string) Inputs {
	return Inputs{
		NodeName:   runOn,
		Nodes:      []*corev1.Node{node("node-a", "10.0.0.1", nil)},
		Namespaces: []*corev1.Namespace{ns("games", nil)},
		Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a"})},
		PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
		Slices: []*discoveryv1.EndpointSlice{
			slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-a", target: "pod-a"}),
		},
		Now: metav1.NewTime(baseTime),
	}
}

func onlyDNAT(t *testing.T, st datapath.State) datapath.DNATRule {
	t.Helper()
	if len(st.DNAT) != 1 {
		t.Fatalf("DNAT = %+v, want one rule", st.DNAT)
	}
	return st.DNAT[0]
}

// TestTranslateLocalPod: the client dials 3000 and the pod listens on 8096. The
// accepting node writes the pod's address and port in one DNAT, and the
// masquerade exemption matches the port the DNAT wrote, which is what keeps the
// client's address.
func TestTranslateLocalPod(t *testing.T) {
	res := Compute(withSlicePortNumber(localWorld("node-a"), 8096))

	d := onlyDNAT(t, res.State)
	if d.Port.First != 3000 || d.ToAddr.String() != "10.244.5.5" || d.ToPort != 8096 {
		t.Errorf("DNAT = %+v, want dport 3000 to 10.244.5.5:8096", d)
	}
	if len(res.State.Exempt) != 1 || res.State.Exempt[0].Port.First != 8096 {
		t.Errorf("Exempt = %+v, want one matching dport 8096 after the DNAT", res.State.Exempt)
	}

	text := datapath.Render(res.State).String()
	if !strings.Contains(text, `tcp dport 3000 counter dnat to 10.244.5.5:8096`) {
		t.Errorf("rendered ruleset lacks the translating DNAT:\n%s", text)
	}
	if !strings.Contains(text, `ip daddr 10.244.5.5 tcp dport 8096 counter snat to ip saddr`) {
		t.Errorf("rendered ruleset lacks the exemption on the translated port:\n%s", text)
	}
}

// TestTranslateRemoteSingle: the request crosses the return link on the port the
// client dialed, the pod's node translates it, and the static mark matches the
// reply by the pod's port.
func TestTranslateRemoteSingle(t *testing.T) {
	acc := Compute(withSlicePortNumber(remoteWorld("node-a"), 8096))
	d := onlyDNAT(t, acc.State)
	if d.Port.First != 3000 || d.ToPort != 0 || !d.ToAddr.IsLinkLocalUnicast() {
		t.Errorf("accepting DNAT = %+v, want dport 3000 to the link's far end, port kept", d)
	}
	if len(acc.State.Exempt) != 1 || acc.State.Exempt[0].Port.First != 3000 {
		t.Errorf("accepting Exempt = %+v, want dport 3000 toward the link", acc.State.Exempt)
	}

	tgt := Compute(withSlicePortNumber(remoteWorld("node-b"), 8096))
	d = onlyDNAT(t, tgt.State)
	if d.Port.First != 3000 || d.ToAddr.String() != "10.244.5.5" || d.ToPort != 8096 {
		t.Errorf("link DNAT = %+v, want dport 3000 to 10.244.5.5:8096", d)
	}
	if len(tgt.State.Mark) != 1 || tgt.State.Mark[0].Port.First != 8096 {
		t.Errorf("Mark = %+v, want the reply matched on sport 8096", tgt.State.Mark)
	}
	if len(tgt.State.Exempt) != 1 || tgt.State.Exempt[0].Port.First != 8096 || !tgt.State.Exempt[0].PortIsSrc {
		t.Errorf("target Exempt = %+v, want sport 8096", tgt.State.Exempt)
	}
}

// TestTranslateMulti: every accepting node keeps the dialed port on its link;
// the pod's node translates on its own interface and on each link, and the
// conntrack restore matches the reply by the pod's port.
func TestTranslateMulti(t *testing.T) {
	for _, n := range []string{"node-a", "node-b"} {
		d := onlyDNAT(t, Compute(withSlicePortNumber(multiWorld(n), 8096)).State)
		if d.Port.First != 3000 || d.ToPort != 0 {
			t.Errorf("%s: DNAT = %+v, want dport 3000 with the port kept for the link", n, d)
		}
	}

	res := Compute(withSlicePortNumber(multiWorld("node-c"), 8096))
	if len(res.State.DNAT) != 3 {
		t.Fatalf("node-c: DNAT = %+v, want its interface plus one per link", res.State.DNAT)
	}
	for _, d := range res.State.DNAT {
		if d.Port.First != 3000 || d.ToAddr.String() != "10.244.9.9" || d.ToPort != 8096 {
			t.Errorf("node-c: DNAT = %+v, want dport 3000 to 10.244.9.9:8096", d)
		}
	}
	if len(res.State.CtLoad) != 1 || res.State.CtLoad[0].Port.First != 8096 {
		t.Errorf("node-c: CtLoad = %+v, want the reply matched on sport 8096", res.State.CtLoad)
	}
}

// TestSamePortNumberRendersAsBefore: a slice listing the dialed port renders
// exactly what a slice listing no number does, on every node and in both modes.
// Every mapping written before translation existed is one of these.
func TestSamePortNumberRendersAsBefore(t *testing.T) {
	worlds := map[string]func(string, ...classOpt) Inputs{"single": remoteWorld, "multi": multiWorld}
	for mode, world := range worlds {
		for _, n := range []string{"node-a", "node-b", "node-c"} {
			before := Compute(world(n)).State
			after := Compute(withSlicePortNumber(world(n), 3000)).State
			if !reflect.DeepEqual(before, after) {
				t.Errorf("%s on %s: state changed with the port number listed\nbefore %+v\nafter  %+v", mode, n, before, after)
			}
		}
	}
}

func accepted(t *testing.T, in Inputs, name string) metav1.Condition {
	t.Helper()
	m := resolvedMappingFor(in, "games", name)
	if m == nil {
		t.Fatalf("no mapping %s", name)
	}
	return m.acceptedCond
}

// TestRangeTranslationRejected: a range reaches the pod on the ports it names,
// so a named port listing another number is refused; the same number is fine.
func TestRangeTranslationRejected(t *testing.T) {
	in := localWorld("node-a")
	in.PortMaps = []*v1alpha1.PortMap{pm("games", "a", 3000, 0, withEndPort(3009))}

	c := accepted(t, withSlicePortNumber(in, 8096), "a")
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonRangeTranslation {
		t.Errorf("range onto 8096: Accepted = %s/%s, want False/%s", c.Status, c.Reason, v1alpha1.ReasonRangeTranslation)
	}

	in = localWorld("node-a")
	in.PortMaps = []*v1alpha1.PortMap{pm("games", "a", 3000, 0, withEndPort(3009))}
	if c := accepted(t, withSlicePortNumber(in, 3000), "a"); c.Status != metav1.ConditionTrue {
		t.Errorf("range onto 3000: Accepted = %s/%s, want True", c.Status, c.Reason)
	}
}

// TestTargetPortInUse: two mappings on one Service port where one translates
// would write two return rules for the same replies, so the later one is
// refused. Two untranslated mappings on one Service port stay accepted.
func TestTargetPortInUse(t *testing.T) {
	in := localWorld("node-a")
	in.PortMaps = []*v1alpha1.PortMap{
		pm("games", "a", 3000, 0),
		pm("games", "b", 3001, 1, withService("a", "svc")),
	}
	in = withSlicePortNumber(in, 8096)
	if c := accepted(t, in, "a"); c.Status != metav1.ConditionTrue {
		t.Errorf("earlier mapping: Accepted = %s/%s, want True", c.Status, c.Reason)
	}
	c := accepted(t, in, "b")
	if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonTargetPortInUse {
		t.Errorf("later mapping: Accepted = %s/%s, want False/%s", c.Status, c.Reason, v1alpha1.ReasonTargetPortInUse)
	}
	if !strings.Contains(c.Message, "games/a") {
		t.Errorf("message %q does not name the mapping holding the port", c.Message)
	}

	in = localWorld("node-a")
	in.Classes = append(in.Classes, class("lan", []string{"node-a"}))
	in.PortMaps = []*v1alpha1.PortMap{
		pm("games", "a", 3000, 0),
		pm("games", "b", 3000, 1, withService("a", "svc"), withClass("lan")),
	}
	in = withSlicePortNumber(in, 3000)
	for _, name := range []string{"a", "b"} {
		if c := accepted(t, in, name); c.Status != metav1.ConditionTrue {
			t.Errorf("untranslated %s: Accepted = %s/%s, want True", name, c.Status, c.Reason)
		}
	}
}
