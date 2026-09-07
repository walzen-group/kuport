package reconcile

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// baseInputs is a single-node world where node-a accepts the "public" class, so
// every PortMap's status is owned by node-a and its Accepted condition is
// observable there.
func baseInputs(pms []*v1alpha1.PortMap, opts ...classOpt) Inputs {
	return Inputs{
		NodeName:   "node-a",
		Nodes:      []*corev1.Node{node("node-a", "10.0.0.1", map[string]string{"edge": "true"})},
		Namespaces: []*corev1.Namespace{ns("games", map[string]string{"public": "yes"})},
		Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a"}, opts...)},
		PortMaps:   pms,
		Now:        metav1.NewTime(baseTime),
	}
}

func acceptedOf(res Result, namespace, name string) metav1.Condition {
	st := res.PortMapStatus[types.NamespacedName{Namespace: namespace, Name: name}]
	return findCond(st.Conditions, v1alpha1.ConditionAccepted)
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name       string
		pm         *v1alpha1.PortMap
		opts       []classOpt
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "in range",
			pm:         pm("games", "a", 3000, 0),
			opts:       []classOpt{withPorts(1024, 65535)},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonValid,
		},
		{
			name:       "below min",
			pm:         pm("games", "a", 1000, 0),
			opts:       []classOpt{withPorts(1024, 65535)},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonPortOutOfRange,
		},
		{
			name:       "above max",
			pm:         pm("games", "a", 40000, 0),
			opts:       []classOpt{withPorts(1024, 30000)},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonPortOutOfRange,
		},
		{
			name:       "reserved",
			pm:         pm("games", "a", 443, 0),
			opts:       []classOpt{withPorts(1, 65535, 80, 443)},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonPortReserved,
		},
		{
			name:       "endPort below port",
			pm:         pm("games", "a", 3000, 0, withEndPort(2999)),
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonInvalidPortRange,
		},
		{
			name:       "range straddling a reserved port",
			pm:         pm("games", "a", 79, 0, withEndPort(81)),
			opts:       []classOpt{withPorts(1, 65535, 80)},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonPortReserved,
		},
		{
			name:       "class not found",
			pm:         pm("games", "a", 3000, 0, withClass("missing")),
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonClassNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compute(baseInputs([]*v1alpha1.PortMap{tt.pm}, tt.opts...))
			got := acceptedOf(res, "games", "a")
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason {
				t.Fatalf("Accepted = %s/%s, want %s/%s", got.Status, got.Reason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

func TestNamespaceGating(t *testing.T) {
	tests := []struct {
		name       string
		nsLabels   map[string]string
		selector   map[string]string // nil means no selector on the class
		nilSel     bool
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "selected",
			nsLabels:   map[string]string{"public": "yes"},
			selector:   map[string]string{"public": "yes"},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonValid,
		},
		{
			name:       "not selected",
			nsLabels:   map[string]string{"public": "no"},
			selector:   map[string]string{"public": "yes"},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1alpha1.ReasonNamespaceNotSelected,
		},
		{
			name:       "nil selector selects all",
			nsLabels:   nil,
			nilSel:     true,
			wantStatus: metav1.ConditionTrue,
			wantReason: v1alpha1.ReasonValid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []classOpt{}
			if !tt.nilSel {
				opts = append(opts, withNamespaceSelector(tt.selector))
			}
			in := Inputs{
				NodeName:   "node-a",
				Nodes:      []*corev1.Node{node("node-a", "10.0.0.1", map[string]string{"edge": "true"})},
				Namespaces: []*corev1.Namespace{ns("games", tt.nsLabels)},
				Classes:    []*v1alpha1.PortMapClass{class("public", []string{"node-a"}, opts...)},
				PortMaps:   []*v1alpha1.PortMap{pm("games", "a", 3000, 0)},
				Now:        metav1.NewTime(baseTime),
			}
			got := acceptedOf(Compute(in), "games", "a")
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason {
				t.Fatalf("Accepted = %s/%s, want %s/%s", got.Status, got.Reason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

func TestConflicts(t *testing.T) {
	t.Run("disjoint intervals coexist", func(t *testing.T) {
		a := pm("games", "a", 3000, 0)
		b := pm("games", "b", 3001, 0)
		res := Compute(baseInputs([]*v1alpha1.PortMap{a, b}))
		if c := acceptedOf(res, "games", "a"); c.Status != metav1.ConditionTrue {
			t.Errorf("a Accepted = %s, want True", c.Status)
		}
		if c := acceptedOf(res, "games", "b"); c.Status != metav1.ConditionTrue {
			t.Errorf("b Accepted = %s, want True", c.Status)
		}
	})

	t.Run("overlapping intervals, earlier wins", func(t *testing.T) {
		early := pm("games", "early", 3000, 0, withEndPort(3010))
		late := pm("games", "late", 3005, 5, withEndPort(3015))
		res := Compute(baseInputs([]*v1alpha1.PortMap{late, early})) // input order reversed on purpose
		if c := acceptedOf(res, "games", "early"); c.Status != metav1.ConditionTrue {
			t.Errorf("early Accepted = %s, want True", c.Status)
		}
		c := acceptedOf(res, "games", "late")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonPortConflict {
			t.Errorf("late Accepted = %s/%s, want False/PortConflict", c.Status, c.Reason)
		}
		if !strings.Contains(c.Message, "games/early") {
			t.Errorf("late message = %q, want it to name games/early", c.Message)
		}
	})

	t.Run("same creationTimestamp, name tiebreak", func(t *testing.T) {
		// Both created at the same second; the name tiebreak must pick the
		// bytewise-earlier namespace/name as the winner, on every agent.
		aaa := pm("games", "aaa", 3000, 0)
		zzz := pm("games", "zzz", 3000, 0)
		res := Compute(baseInputs([]*v1alpha1.PortMap{zzz, aaa}))
		if c := acceptedOf(res, "games", "aaa"); c.Status != metav1.ConditionTrue {
			t.Errorf("aaa Accepted = %s, want True (bytewise-earlier name wins)", c.Status)
		}
		c := acceptedOf(res, "games", "zzz")
		if c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonPortConflict {
			t.Errorf("zzz Accepted = %s/%s, want False/PortConflict", c.Status, c.Reason)
		}
		if !strings.Contains(c.Message, "games/aaa") {
			t.Errorf("zzz message = %q, want it to name games/aaa", c.Message)
		}
	})

	t.Run("different protocols on the same port coexist", func(t *testing.T) {
		tcp := pm("games", "tcp", 3000, 0, withProto(v1alpha1.ProtocolTCP))
		udp := pm("games", "udp", 3000, 0, withProto(v1alpha1.ProtocolUDP))
		res := Compute(baseInputs([]*v1alpha1.PortMap{tcp, udp}))
		if c := acceptedOf(res, "games", "tcp"); c.Status != metav1.ConditionTrue {
			t.Errorf("tcp Accepted = %s, want True", c.Status)
		}
		if c := acceptedOf(res, "games", "udp"); c.Status != metav1.ConditionTrue {
			t.Errorf("udp Accepted = %s, want True", c.Status)
		}
	})
}
