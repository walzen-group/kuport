package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	"github.com/walzen-group/kuport/internal/datapath"
)

// baseTime anchors every fixture's timestamps so tests never read a real clock.
var baseTime = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func fixedNow() metav1.Time { return metav1.NewTime(baseTime) }

// testScheme registers the core and kuport types the fake client serves.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("kuport scheme: %v", err)
	}
	return s
}

// fakeDatapath records the calls the reconciler makes so a test can assert Apply
// ran and Teardown ran on shutdown, all without a kernel.
type fakeDatapath struct {
	applied    []datapath.State
	teardowns  int
	applyErr   error
	teardownFn func() error
}

func (d *fakeDatapath) Apply(_ context.Context, state datapath.State) error {
	d.applied = append(d.applied, state)
	return d.applyErr
}

func (d *fakeDatapath) Teardown(context.Context) error {
	d.teardowns++
	if d.teardownFn != nil {
		return d.teardownFn()
	}
	return nil
}

// fakeHost is a scripted Host: fixed tunnel mode, port, MTU and per-interface
// addresses, so status and reconcile tests need no netlink.
type fakeHost struct {
	tunnelOK    bool
	tunnelKnown bool
	tunnelErr   error
	cniPort     int
	underlay    int
	underlayErr error
	ifaceAddr   map[string]string
}

func (h *fakeHost) TunnelMode(context.Context) (bool, bool, error) {
	return h.tunnelOK, h.tunnelKnown, h.tunnelErr
}

func (h *fakeHost) CNITunnelPort() int {
	if h.cniPort == 0 {
		return ciliumTunnelPort
	}
	return h.cniPort
}

func (h *fakeHost) UnderlayMTU(string) (int, error) { return h.underlay, h.underlayErr }

func (h *fakeHost) InterfaceAddr(name string) (string, error) {
	if a, ok := h.ifaceAddr[name]; ok {
		return a, nil
	}
	return "", net.UnknownNetworkError(name)
}

// fakeNetlink is a minimal datapath.NetlinkConn for the host checks: it serves a
// fixed set of links, each with an MTU and a set of IPv4 addresses. Only the
// read methods the host checks call are meaningful; the mutating methods return
// nil so the interface is satisfied.
type fakeNetlink struct {
	links map[string]fakeLink
}

type fakeLink struct {
	index int
	mtu   int
	addrs []string
}

func (f fakeNetlink) LinkByName(name string) (netlink.Link, error) {
	l, ok := f.links[name]
	if !ok {
		return nil, netlink.LinkNotFoundError{}
	}
	return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: l.mtu, Index: l.index}}, nil
}

func (f fakeNetlink) LinkList() ([]netlink.Link, error) {
	var out []netlink.Link
	for name, l := range f.links {
		out = append(out, &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: l.mtu, Index: l.index}})
	}
	return out, nil
}

func (f fakeNetlink) AddrList(link netlink.Link, _ int) ([]netlink.Addr, error) {
	l, ok := f.links[link.Attrs().Name]
	if !ok {
		return nil, nil
	}
	var out []netlink.Addr
	for _, a := range l.addrs {
		out = append(out, netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(a), Mask: net.CIDRMask(24, 32)}})
	}
	return out, nil
}

func (fakeNetlink) LinkAdd(netlink.Link) error                { return nil }
func (fakeNetlink) LinkDel(netlink.Link) error                { return nil }
func (fakeNetlink) LinkSetUp(netlink.Link) error              { return nil }
func (fakeNetlink) AddrAdd(netlink.Link, *netlink.Addr) error { return nil }
func (fakeNetlink) RuleAdd(*netlink.Rule) error               { return nil }
func (fakeNetlink) RuleDel(*netlink.Rule) error               { return nil }
func (fakeNetlink) RuleList(int) ([]netlink.Rule, error)      { return nil, nil }
func (fakeNetlink) RouteAdd(*netlink.Route) error             { return nil }
func (fakeNetlink) RouteDel(*netlink.Route) error             { return nil }

func (fakeNetlink) RouteListFiltered(int, *netlink.Route, uint64) ([]netlink.Route, error) {
	return nil, nil
}

// node builds a Node with an InternalIP and labels.
func node(name, internalIP string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: internalIP}},
		},
	}
}
