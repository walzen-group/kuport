package agent

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/walzen-group/kuport/internal/datapath"
)

// ciliumTunnelPort is the UDP port Cilium's own VXLAN tunnel uses. A return-path
// link must not share it, or the two tunnels collide on the wire.
const ciliumTunnelPort = 8472

// realHost answers the host checks against the live kernel and the in-cluster
// API server. The ConfigMap read goes through an uncached reader so the agent
// does not have to watch every ConfigMap in the cluster; the interface reads go
// through the same netlink handle the datapath uses.
type realHost struct {
	reader client.Reader // uncached; reads cilium-config directly
	nl     datapath.NetlinkConn
}

// NewHost builds the production Host from an uncached API reader and a netlink
// handle. The reader must be able to get ConfigMaps in kube-system, an RBAC
// grant the agent's ClusterRole carries.
func NewHost(reader client.Reader, nl datapath.NetlinkConn) Host {
	return &realHost{reader: reader, nl: nl}
}

// TunnelMode reads Cilium's config and reports whether it routes through a
// tunnel. The current key is routing-mode; older Cilium set tunnel to vxlan or
// geneve. When the ConfigMap is absent, Cilium is not the CNI, and when it
// carries no recognised key the mode cannot be read; both return known=false so
// the caller refuses no class on a mode it could not establish.
func (h *realHost) TunnelMode(ctx context.Context) (ok bool, known bool, err error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "kube-system", Name: "cilium-config"}
	if err := h.reader.Get(ctx, key, &cm); err != nil {
		if kerrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}
	if mode, present := cm.Data["routing-mode"]; present {
		return mode == "tunnel", true, nil
	}
	if tunnel, present := cm.Data["tunnel"]; present {
		return tunnel == "vxlan" || tunnel == "geneve", true, nil
	}
	return false, false, nil
}

// CNITunnelPort is the CNI's tunnel UDP port, 8472 for Cilium.
func (h *realHost) CNITunnelPort() int { return ciliumTunnelPort }

// UnderlayMTU reads the MTU of the interface bearing the node's InternalIP, the
// underlay every return link rides. datapath.LinkMTU turns it into the payload a
// link can carry.
func (h *realHost) UnderlayMTU(internalIP string) (int, error) {
	links, err := h.nl.LinkList()
	if err != nil {
		return 0, err
	}
	for _, l := range links {
		addrs, err := h.nl.AddrList(l, unix.AF_INET)
		if err != nil {
			return 0, err
		}
		for _, a := range addrs {
			if a.IP != nil && a.IP.String() == internalIP {
				return l.Attrs().MTU, nil
			}
		}
	}
	return 0, fmt.Errorf("no interface bears InternalIP %s", internalIP)
}

// InterfaceAddr returns the first IPv4 address on a named interface, the address
// a mapping's port answers on when this node accepts on that interface.
func (h *realHost) InterfaceAddr(name string) (string, error) {
	link, err := h.nl.LinkByName(name)
	if err != nil {
		return "", err
	}
	addrs, err := h.nl.AddrList(link, unix.AF_INET)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if a.IP != nil {
			return a.IP.String(), nil
		}
	}
	return "", fmt.Errorf("interface %s has no IPv4 address", name)
}
