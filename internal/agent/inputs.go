package agent

import (
	"context"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
	kreconcile "github.com/walzen-group/kuport/internal/reconcile"
)

// buildInputs reads the whole watched world out of the informer cache into the
// snapshot reconcile.Compute consumes. The lists come from the cache, so this is
// cheap and does no API round-trip; the reconcile then sees a consistent
// point-in-time view of every object it is allowed to look at.
func (r *Reconciler) buildInputs(ctx context.Context) (kreconcile.Inputs, error) {
	var (
		nodes   corev1.NodeList
		nss     corev1.NamespaceList
		classes v1alpha1.PortMapClassList
		pms     v1alpha1.PortMapList
		slices  discoveryv1.EndpointSliceList
	)
	for _, list := range []client.ObjectList{&nodes, &nss, &classes, &pms, &slices} {
		if err := r.Client.List(ctx, list); err != nil {
			return kreconcile.Inputs{}, err
		}
	}

	in := kreconcile.Inputs{
		NodeName: r.NodeName,
		Now:      r.now(),
	}
	for i := range nodes.Items {
		in.Nodes = append(in.Nodes, &nodes.Items[i])
	}
	for i := range nss.Items {
		in.Namespaces = append(in.Namespaces, &nss.Items[i])
	}
	for i := range classes.Items {
		in.Classes = append(in.Classes, &classes.Items[i])
	}
	for i := range pms.Items {
		in.PortMaps = append(in.PortMaps, &pms.Items[i])
	}
	for i := range slices.Items {
		in.Slices = append(in.Slices, &slices.Items[i])
	}
	in.InterfaceAddrs, in.InterfaceAddrs6 = r.resolveInterfaces(in.Classes)
	in.IPv6Forwarding = r.ipv6Forwarding()
	in.LocalAddrs = r.localAddrs()
	return in, nil
}

// localAddrs reads the addresses the node holds. A failed read steers nothing
// this pass: a mapping still answers on a node without a host firewall, and
// the error is logged for the node that has one.
func (r *Reconciler) localAddrs() []netip.Addr {
	addrs, err := r.Host.LocalAddrs()
	if err != nil {
		r.Log.Error(err, "cannot read the node's addresses; mapped packets are not steered past a host firewall this pass")
		return nil
	}
	return addrs
}

// ipv6Forwarding reads whether the host forwards IPv6. An unreadable sysctl
// counts as off: the node then programs IPv4 alone, which is what it can
// deliver. A change is logged once, because a node that is no mapping's
// serving node has no status that would show it.
func (r *Reconciler) ipv6Forwarding() bool {
	on, err := r.Host.IPv6Forwarding()
	if err != nil {
		on = false
	}
	if r.lastIPv6Fwd == nil || *r.lastIPv6Fwd != on {
		if on {
			r.Log.Info("IPv6 forwarding is on; mappings with an IPv6 endpoint are delivered over IPv6 here")
		} else {
			r.Log.Info("IPv6 forwarding is off; this node delivers IPv4 alone",
				"sysctl", "net.ipv6.conf.all.forwarding", "readError", errString(err))
		}
		r.lastIPv6Fwd = &on
	}
	return on
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// resolveInterfaces reads the interfaces the classes give this node off the
// host, each one once, for the snapshot Compute turns into class node rows.
// Interfaces a class gives other nodes are never read here, so a class naming
// eth0 on one node and enp1s0 on another leaves each node reading only its
// own. An interface the host cannot resolve is absent from the first map;
// Compute reports it as a not-ready row naming it, never as a skipped row. The
// second map holds the IPv6 addresses of those that have one.
func (r *Reconciler) resolveInterfaces(classes []*v1alpha1.PortMapClass) (v4, v6 map[string]string) {
	seen := map[string]bool{}
	for _, c := range classes {
		for _, iface := range c.Spec.Nodes[r.NodeName].Interfaces {
			if seen[iface] {
				continue
			}
			seen[iface] = true
			if addr, err := r.Host.InterfaceAddr(iface); err == nil {
				if v4 == nil {
					v4 = map[string]string{}
				}
				v4[iface] = addr
			}
			if addr, err := r.Host.InterfaceAddr6(iface); err == nil {
				if v6 == nil {
					v6 = map[string]string{}
				}
				v6[iface] = addr
			}
		}
	}
	return v4, v6
}
