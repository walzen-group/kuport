package datapath

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// want is the link a create was refused for, shared by the cases below.
func holderWant() Link {
	return Link{
		Name:       "kup-9b26b4ae",
		VNI:        4242,
		Port:       4790,
		LocalAddr:  netip.MustParseAddr("100.64.93.143"),
		RemoteAddr: netip.MustParseAddr("100.64.211.214"),
	}
}

func vxlanDev(name string, vni, port int) *netlink.Vxlan {
	return &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		VxlanId:   vni,
		Port:      port,
	}
}

// A device already answering for the VNI on the same port is the whole reason
// the kernel returns EEXIST, so its name has to reach the operator.
func TestVxlanHoldersNamesTheCollidingDevice(t *testing.T) {
	f := newFakeNL()
	f.links["someone-elses"] = vxlanDev("someone-elses", 4242, 4790)
	f.links["cilium_vxlan"] = vxlanDev("cilium_vxlan", 2, 8472)

	got := vxlanHolders(f, holderWant())

	if !strings.Contains(got, "someone-elses") {
		t.Errorf("the colliding device is not named: %q", got)
	}
	if strings.Contains(got, "cilium_vxlan") {
		t.Errorf("a device on another port is reported as a collision: %q", got)
	}
}

// With no collision the message still lists what is there, because an EEXIST
// with nothing holding the VNI is a different problem and has to look like one.
func TestVxlanHoldersReportsNoCollision(t *testing.T) {
	f := newFakeNL()
	f.links["cilium_vxlan"] = vxlanDev("cilium_vxlan", 2, 8472)

	got := vxlanHolders(f, holderWant())

	if !strings.Contains(got, "no device holds vni 4242") {
		t.Errorf("absence of a collision is not stated: %q", got)
	}
	if !strings.Contains(got, "cilium_vxlan") {
		t.Errorf("the other vxlan devices are not listed: %q", got)
	}
}

// A node with no VXLAN at all says so, which separates a kernel that refused
// for another reason from a device kuport could have found.
func TestVxlanHoldersReportsAnEmptyNode(t *testing.T) {
	f := newFakeNL()

	got := vxlanHolders(f, holderWant())

	if !strings.Contains(got, "no vxlan device is present") {
		t.Errorf("an empty node is not stated: %q", got)
	}
}

// A non-VXLAN link is not an underlay kuport can read, and it must not be
// reported as one.
func TestVxlanHoldersSkipsNonVxlanLinks(t *testing.T) {
	f := newFakeNL()
	f.links["eth0"] = &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}

	got := vxlanHolders(f, holderWant())

	if strings.Contains(got, "eth0") {
		t.Errorf("a non-vxlan link is reported: %q", got)
	}
}
