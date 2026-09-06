package agent

import (
	"context"

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
	return in, nil
}
