package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/walzen-group/kuport/internal/datapath"
)

func ciliumConfig(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
		Data:       data,
	}
}

// TestTunnelMode drives the tunnel-mode check against fabricated ConfigMaps: the
// current routing-mode key, the legacy tunnel key, an unrecognised config, and
// an absent ConfigMap. The absent and unrecognised cases must report known=false
// so no class is refused on a mode that could not be read.
func TestTunnelMode(t *testing.T) {
	cases := []struct {
		name          string
		cm            *corev1.ConfigMap
		wantOK, known bool
	}{
		{"routing-mode tunnel", ciliumConfig(map[string]string{"routing-mode": "tunnel"}), true, true},
		{"routing-mode native", ciliumConfig(map[string]string{"routing-mode": "native"}), false, true},
		{"legacy tunnel vxlan", ciliumConfig(map[string]string{"tunnel": "vxlan"}), true, true},
		{"legacy tunnel geneve", ciliumConfig(map[string]string{"tunnel": "geneve"}), true, true},
		{"legacy tunnel disabled", ciliumConfig(map[string]string{"tunnel": "disabled"}), false, true},
		{"config present, no mode key", ciliumConfig(map[string]string{"cluster-name": "x"}), false, false},
		{"config absent", nil, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(testScheme(t))
			if tc.cm != nil {
				b = b.WithObjects(tc.cm)
			}
			h := NewHost(b.Build(), fakeNetlink{})

			ok, known, err := h.TunnelMode(context.Background())
			if err != nil {
				t.Fatalf("TunnelMode: %v", err)
			}
			if ok != tc.wantOK || known != tc.known {
				t.Errorf("TunnelMode = (ok=%v, known=%v), want (ok=%v, known=%v)", ok, known, tc.wantOK, tc.known)
			}
		})
	}
}

// TestCNITunnelPort pins the CNI tunnel port the collision check compares against.
func TestCNITunnelPort(t *testing.T) {
	h := NewHost(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), fakeNetlink{})
	if got := h.CNITunnelPort(); got != 8472 {
		t.Errorf("CNITunnelPort = %d, want 8472", got)
	}
}

// TestUnderlayMTU finds the InternalIP-bearing interface's MTU and confirms
// datapath.LinkMTU carries the documented 50-byte overhead: 1350 underlay leaves
// a 1300-byte link, the zero-margin case the design was built against.
func TestUnderlayMTU(t *testing.T) {
	nl := fakeNetlink{links: map[string]fakeLink{
		"lo":   {index: 1, mtu: 65536, addrs: []string{"127.0.0.1"}},
		"eth0": {index: 2, mtu: 1350, addrs: []string{"10.0.0.5"}},
	}}
	h := NewHost(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), nl)

	mtu, err := h.UnderlayMTU("10.0.0.5")
	if err != nil {
		t.Fatalf("UnderlayMTU: %v", err)
	}
	if mtu != 1350 {
		t.Errorf("UnderlayMTU = %d, want 1350", mtu)
	}
	if got := datapath.LinkMTU(mtu); got != 1300 {
		t.Errorf("LinkMTU = %d, want 1300", got)
	}

	if _, err := h.UnderlayMTU("10.9.9.9"); err == nil {
		t.Error("UnderlayMTU on an unowned address: want error, got nil")
	}
}

// TestInterfaceAddr resolves a published interface's address, which is how the
// status writer fills a Published row for its own node.
func TestInterfaceAddr(t *testing.T) {
	nl := fakeNetlink{links: map[string]fakeLink{
		"eth0": {index: 2, mtu: 1500, addrs: []string{"192.0.2.10"}},
		"eth1": {index: 3, mtu: 1500},
	}}
	h := NewHost(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), nl)

	addr, err := h.InterfaceAddr("eth0")
	if err != nil {
		t.Fatalf("InterfaceAddr: %v", err)
	}
	if addr != "192.0.2.10" {
		t.Errorf("InterfaceAddr = %q, want 192.0.2.10", addr)
	}
	if _, err := h.InterfaceAddr("eth1"); err == nil {
		t.Error("InterfaceAddr on an address-less interface: want error, got nil")
	}
	if _, err := h.InterfaceAddr("missing"); err == nil {
		t.Error("InterfaceAddr on a missing interface: want error, got nil")
	}
}
