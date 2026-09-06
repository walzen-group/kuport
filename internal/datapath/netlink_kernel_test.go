package datapath

import (
	"bytes"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// netnsMarker is logged by the inner run the moment it is inside the fresh
// network namespace. It lets the outer run tell "this environment cannot open
// a netns" (skip) apart from "the update failed on the real kernel" (fail).
const netnsMarker = "KUPT-NS-STARTED"

// TestApplyLinksUpdatesUnderlayOnRealKernel is the F7 end-to-end proof: it
// re-executes itself under `unshare -rn` (the repo's netns harness pattern)
// and runs applyLinks against the real kernel — create, then move the local
// address out from under the existing link and show one apply reconciles the
// underlay in place: same index, the /31 intact, the link never taken down,
// nothing deleted. It then changes the remaining params one at a time on the
// live interface, which also answers whether the kernel accepts each change
// while the device is up. Where the kernel cannot open a netns the test
// skips.
func TestApplyLinksUpdatesUnderlayOnRealKernel(t *testing.T) {
	if os.Getenv("KUPORT_NETNS_TEST") == "1" {
		t.Log(netnsMarker)
		applyLinksRealKernel(t)
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not on PATH; real-kernel link proof skipped")
	}
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("cannot open a fresh netns here: %v\n%s", err, out)
	}

	cmd := exec.Command("unshare", "-rn", os.Args[0], "-test.run=TestApplyLinksUpdatesUnderlayOnRealKernel", "-test.v")
	cmd.Env = append(os.Environ(), "KUPORT_NETNS_TEST=1")
	out, err := cmd.CombinedOutput()
	if !bytes.Contains(out, []byte(netnsMarker)) {
		t.Skipf("re-exec under unshare never reached the netns body: %v\n%s", err, out)
	}
	if err != nil {
		t.Fatalf("real-kernel run failed: %v\n%s", err, out)
	}
	t.Logf("real-kernel run:\n%s", out)
}

// liveUnderlay reads back the kernel's view of the link and re-pins the
// invariants the F7 update must never break: the device index is the one
// captured at creation (idx, >0), the link is still up, the /31 is still on
// it, and the host still holds exactly one kup-* device.
func liveUnderlay(t *testing.T, nl NetlinkConn, want Link, idx int) *netlink.Vxlan {
	t.Helper()
	l, err := nl.LinkByName(want.Name)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", want.Name, err)
	}
	v, ok := l.(*netlink.Vxlan)
	if !ok {
		t.Fatalf("%s is %T on the real kernel, want *netlink.Vxlan", want.Name, l)
	}
	if idx > 0 && v.Attrs().Index != idx {
		t.Fatalf("device index moved %d -> %d: the update must not recreate the link", idx, v.Attrs().Index)
	}
	if v.Attrs().Flags&net.FlagUp == 0 {
		t.Errorf("%s is down after the update; the live interface must stay up", want.Name)
	}
	addrs, err := nl.AddrList(v, 0) // 0 = AF_UNSPEC: dump every family, the v4 is what we hold
	if err != nil {
		t.Fatalf("AddrList: %v", err)
	}
	addr, err := netlink.ParseAddr(want.LinkAddr.String())
	if err != nil {
		t.Fatalf("parse link addr: %v", err)
	}
	if !addrPresent(addrs, addr) {
		t.Errorf("%s lost its /31 during the update: have %v", want.Name, addrs)
	}
	all, err := nl.LinkList()
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	var n int
	for _, other := range all {
		if strings.HasPrefix(other.Attrs().Name, linkPrefix) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("kup-* devices on host = %d, want 1 (no deletes, no duplicates)", n)
	}
	return v
}

func applyLinksRealKernel(t *testing.T) {
	nl := realNetlink{}

	// A fresh netns holds only a down lo, and a VXLAN can bind only a local
	// address the host owns: give it the candidates and bring lo up.
	lo, err := nl.LinkByName("lo")
	if err != nil {
		t.Fatalf("lo: %v", err)
	}
	if err := nl.LinkSetUp(lo); err != nil {
		t.Fatalf("lo up: %v", err)
	}
	for _, cidr := range []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32"} {
		a, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("parse %s: %v", cidr, err)
		}
		if err := nl.AddrAdd(lo, a); err != nil {
			t.Fatalf("addr add %s: %v", cidr, err)
		}
	}

	desired := Link{
		Name:       "kup-kern0000",
		VNI:        4242,
		Port:       4790,
		LocalAddr:  netip.MustParseAddr("10.0.0.1"),
		RemoteAddr: netip.MustParseAddr("10.0.0.9"),
		LinkAddr:   netip.MustParsePrefix("169.254.77.1/31"),
	}
	if err := applyLinks(nl, []Link{desired}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	created := liveUnderlay(t, nl, desired, 0)
	if !created.SrcAddr.Equal(desired.LocalAddr.AsSlice()) {
		t.Fatalf("created link has local %v, want %v", created.SrcAddr, desired.LocalAddr)
	}
	idx := created.Attrs().Index

	// The F7 event: the node's InternalIP moved. One apply, no delete.
	desired.LocalAddr = netip.MustParseAddr("10.0.0.2")
	if err := applyLinks(nl, []Link{desired}); err != nil {
		t.Fatalf("apply after address change: %v", err)
	}
	updated := liveUnderlay(t, nl, desired, idx)
	if !updated.SrcAddr.Equal(desired.LocalAddr.AsSlice()) {
		t.Errorf("after address change the kernel holds local %v, want %v", updated.SrcAddr, desired.LocalAddr)
	}
	// A re-read of the kernel's own dump must satisfy the decision as a
	// keep: this is what keeps the next apply a no-op, not an update loop.
	if got := linkActionFor(updated, desired); got != linkKeep {
		t.Errorf("kernel dump of the link we just wrote re-reads as action %d, want keep", got)
	}

	// The rest of the params, one at a time, on the live interface.
	// retarget=true pins the kernel's live in-place change; identity
	// params (vni, port) pin the rebuild path: the name is kept, the
	// device index moves, and the same apply restores the /31.
	steps := []struct {
		name     string
		mutate   func()
		retarget bool
	}{
		{"remote", func() { desired.RemoteAddr = netip.MustParseAddr("10.0.0.10") }, true},
		{"vni", func() { desired.VNI = 7777 }, false},
		{"port", func() { desired.Port = 12345 }, false},
		{"local again", func() { desired.LocalAddr = netip.MustParseAddr("10.0.0.3") }, true},
	}
	for _, s := range steps {
		before := idx
		s.mutate()
		if err := applyLinks(nl, []Link{desired}); err != nil {
			t.Errorf("apply after %s change: %v", s.name, err)
			continue
		}
		v := liveUnderlay(t, nl, desired, 0)
		if s.retarget && v.Attrs().Index != before {
			t.Errorf("after %s change the index moved %d -> %d: endpoints must retarget in place",
				s.name, before, v.Attrs().Index)
		}
		if !s.retarget && v.Attrs().Index == before {
			t.Errorf("after %s change the index held at %d: an identity change must rebuild", s.name, before)
		}
		idx = v.Attrs().Index
		if !v.SrcAddr.Equal(desired.LocalAddr.AsSlice()) || !v.Group.Equal(desired.RemoteAddr.AsSlice()) ||
			v.VxlanId != int(desired.VNI) || v.Port != int(desired.Port) {
			t.Errorf("after %s change the kernel holds local=%v group=%v vni=%d port=%d, want %v %v %d %d",
				s.name, v.SrcAddr, v.Group, v.VxlanId, v.Port,
				desired.LocalAddr, desired.RemoteAddr, desired.VNI, desired.Port)
		}
		if got := linkActionFor(v, desired); got != linkKeep {
			t.Errorf("after %s change the fresh dump re-reads as action %d, want keep", s.name, got)
		}
	}
}
