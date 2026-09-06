package reconcile

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// baseTime anchors every fixture's timestamps so tests never read a real clock.
var baseTime = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// node builds a Node with an InternalIP and labels.
func node(name, internalIP string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: internalIP},
			},
		},
	}
}

// ns builds a Namespace with labels.
func ns(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// classOpt mutates a class during construction.
type classOpt func(*v1alpha1.PortMapClass)

// class builds a PortMapClass selecting nodes by the given matchLabels, on one
// interface, with a Vxlan return path and the default subnet.
func class(name string, nodeMatch map[string]string, opts ...classOpt) *v1alpha1.PortMapClass {
	c := &v1alpha1.PortMapClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PortMapClassSpec{
			NodeSelector: metav1.LabelSelector{MatchLabels: nodeMatch},
			Interfaces:   []string{"eth0"},
			ReturnPath:   v1alpha1.ReturnPath{Mode: v1alpha1.ReturnPathVxlan},
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func withInterfaces(ifaces ...string) classOpt {
	return func(c *v1alpha1.PortMapClass) { c.Spec.Interfaces = ifaces }
}

func withPorts(min, max int32, reserved ...int32) classOpt {
	return func(c *v1alpha1.PortMapClass) {
		c.Spec.Ports = &v1alpha1.PortRange{Min: min, Max: max, Reserved: reserved}
	}
}

func withNamespaceSelector(match map[string]string) classOpt {
	return func(c *v1alpha1.PortMapClass) {
		c.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: match}
	}
}

func withMode(mode v1alpha1.ReturnPathMode) classOpt {
	return func(c *v1alpha1.PortMapClass) { c.Spec.ReturnPath.Mode = mode }
}

func withSubnet(cidr string) classOpt {
	return func(c *v1alpha1.PortMapClass) {
		if c.Spec.ReturnPath.Vxlan == nil {
			c.Spec.ReturnPath.Vxlan = &v1alpha1.VxlanConfig{}
		}
		c.Spec.ReturnPath.Vxlan.Subnet = cidr
	}
}

func withLinks(links ...v1alpha1.LinkAllocation) classOpt {
	return func(c *v1alpha1.PortMapClass) { c.Status.Links = links }
}

func withNodeRows(rows ...v1alpha1.NodeStatus) classOpt {
	return func(c *v1alpha1.PortMapClass) { c.Status.Nodes = rows }
}

// pmOpt mutates a PortMap during construction.
type pmOpt func(*v1alpha1.PortMap)

// pm builds a PortMap in namespace/name asking for a single TCP port behind a
// Service named after its own name on port "svc". createdOffset seconds are
// added to baseTime so ordering is explicit.
func pm(namespace, name string, port int32, createdOffset int, opts ...pmOpt) *v1alpha1.PortMap {
	p := &v1alpha1.PortMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			Generation:        1,
			CreationTimestamp: metav1.NewTime(baseTime.Add(time.Duration(createdOffset) * time.Second)),
		},
		Spec: v1alpha1.PortMapSpec{
			ClassName: "public",
			Protocol:  v1alpha1.ProtocolTCP,
			Port:      port,
			ServiceRef: v1alpha1.ServiceRef{
				Name: name,
				Port: "svc",
			},
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func withClass(name string) pmOpt {
	return func(p *v1alpha1.PortMap) { p.Spec.ClassName = name }
}

func withProto(proto v1alpha1.Protocol) pmOpt {
	return func(p *v1alpha1.PortMap) { p.Spec.Protocol = proto }
}

func withEndPort(end int32) pmOpt {
	return func(p *v1alpha1.PortMap) { p.Spec.EndPort = ptr(end) }
}

func withService(name, port string) pmOpt {
	return func(p *v1alpha1.PortMap) { p.Spec.ServiceRef = v1alpha1.ServiceRef{Name: name, Port: port} }
}

func withGeneration(g int64) pmOpt {
	return func(p *v1alpha1.PortMap) { p.Generation = g }
}

// endpointSpec describes one endpoint for slice construction.
type endpointSpec struct {
	addr     string
	node     string
	target   string
	ready    *bool // nil means the field is absent (treated as ready)
	notReady bool  // sets ready=false explicitly
}

// slice builds an IPv4 EndpointSlice for a Service in a namespace, serving the
// port name "svc", with the given endpoints.
func slice(namespace, serviceName string, eps ...endpointSpec) *discoveryv1.EndpointSlice {
	return slicePort(namespace, serviceName, "svc", eps...)
}

// slicePort is slice with an explicit served port name.
func slicePort(namespace, serviceName, portName string, eps ...endpointSpec) *discoveryv1.EndpointSlice {
	sl := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      serviceName + "-abcde",
			Labels:    map[string]string{serviceNameLabel: serviceName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr(portName)}},
	}
	for _, e := range eps {
		ep := discoveryv1.Endpoint{Addresses: []string{e.addr}}
		if e.node != "" {
			ep.NodeName = ptr(e.node)
		}
		if e.target != "" {
			ep.TargetRef = &corev1.ObjectReference{Kind: "Pod", Name: e.target}
		}
		switch {
		case e.notReady:
			ep.Conditions.Ready = ptr(false)
		case e.ready != nil:
			ep.Conditions.Ready = e.ready
		}
		sl.Endpoints = append(sl.Endpoints, ep)
	}
	return sl
}

// findCond returns the condition of the given type, or a zero condition.
func findCond(conds []metav1.Condition, condType string) metav1.Condition {
	for _, c := range conds {
		if c.Type == condType {
			return c
		}
	}
	return metav1.Condition{}
}

// resolvedMappingFor re-runs the resolution passes and returns the fully
// resolved decision for one PortMap, so an agreement test can compare the
// chosen endpoint, serving node and settled Programmed condition across
// viewpoints even where the visible Result of a non-owner carries nothing.
// The gated condition is settled after the slot allocation, exactly as
// Compute orders them.
func resolvedMappingFor(in Inputs, namespace, name string) *mapping {
	idx := newIndex(in)
	mappings := resolveMappings(in, idx)
	needed := neededPairsByClass(mappings)
	alloc := map[string]classAlloc{}
	for _, c := range in.Classes {
		alloc[c.Name] = allocateClass(c, needed[c.Name])
	}
	resolveProgrammed(in, idx, mappings, alloc)
	for _, m := range mappings {
		if m.pm.Namespace == namespace && m.pm.Name == name {
			return m
		}
	}
	return nil
}
