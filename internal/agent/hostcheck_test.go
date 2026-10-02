package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// TestInterfaceAddr6 resolves the IPv6 address a client dials on an interface.
// Every interface with IPv6 on carries a link-local address, often listed
// first, and no client outside the link can reach it, so it never counts.
func TestInterfaceAddr6(t *testing.T) {
	nl := fakeNetlink{links: map[string]fakeLink{
		"wt0":  {index: 2, mtu: 1280, addrs: []string{"100.64.93.143", "fe80::1", "fd7a:115c:a1e0::9"}},
		"eth1": {index: 3, mtu: 1500, addrs: []string{"192.0.2.10", "fe80::2"}},
	}}
	h := NewHost(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), nl)

	addr, err := h.InterfaceAddr6("wt0")
	if err != nil {
		t.Fatalf("InterfaceAddr6: %v", err)
	}
	if addr != "fd7a:115c:a1e0::9" {
		t.Errorf("InterfaceAddr6 = %q, want fd7a:115c:a1e0::9", addr)
	}
	if v4, _ := h.InterfaceAddr("wt0"); v4 != "100.64.93.143" {
		t.Errorf("InterfaceAddr = %q beside an ipv6 address, want 100.64.93.143", v4)
	}
	if a, err := h.InterfaceAddr6("eth1"); err == nil {
		t.Errorf("InterfaceAddr6 on a link-local-only interface = %q, want an error", a)
	}
}

// TestLocalAddrs lists every address a client could dial across the node's
// interfaces, both families, and leaves out loopback and link-local.
func TestLocalAddrs(t *testing.T) {
	nl := fakeNetlink{links: map[string]fakeLink{
		"lo":     {index: 1, mtu: 65536, addrs: []string{"127.0.0.1", "::1"}},
		"ens3":   {index: 2, mtu: 1500, addrs: []string{"152.53.141.215", "2a0a:4cc0:c1:468::48", "fe80::74b9"}},
		"dummy0": {index: 3, mtu: 1500, addrs: []string{"10.10.111.3", "169.254.116.108"}},
	}}
	h := NewHost(fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), nl)

	addrs, err := h.LocalAddrs()
	if err != nil {
		t.Fatalf("LocalAddrs: %v", err)
	}
	var got []string
	for _, a := range addrs {
		got = append(got, a.String())
	}
	sort.Strings(got)
	want := []string{"10.10.111.3", "152.53.141.215", "2a0a:4cc0:c1:468::48"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LocalAddrs = %q, want %q", got, want)
	}
}

// TestIPv6Forwarding reads the sysctl the way the kernel writes it, with a
// trailing newline.
func TestIPv6Forwarding(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    bool
	}{
		{"1\n", true},
		{"0\n", false},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "sys/net/ipv6/conf/all")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "forwarding"), []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		h := &realHost{proc: dir}
		got, err := h.IPv6Forwarding()
		if err != nil {
			t.Fatalf("IPv6Forwarding: %v", err)
		}
		if got != tc.want {
			t.Errorf("IPv6Forwarding with %q = %v, want %v", tc.content, got, tc.want)
		}
	}

	// A kernel without IPv6 has no such file.
	if _, err := (&realHost{proc: t.TempDir()}).IPv6Forwarding(); err == nil {
		t.Error("IPv6Forwarding with no sysctl: want an error")
	}
}
