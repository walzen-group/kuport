package reconcile

import (
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/walzen-group/kuport/internal/api/v1alpha1"
)

// TestTwoMappingsOnOnePodShareOneLinkOnce: a TCP and a UDP mapping to one pod
// on another node use one link. The pod's node must emit the link, its routing
// rules and its routes once each: a second identical rule makes the kernel
// answer the add with "file exists", and the apply fails.
func TestTwoMappingsOnOnePodShareOneLinkOnce(t *testing.T) {
	in := remoteWorld("node-b")
	in.PortMaps = []*v1alpha1.PortMap{
		pm("games", "tcp", 3000, 0, withService("a", "svc")),
		pm("games", "udp", 3000, 1, withService("a", "svc"), withProto(v1alpha1.ProtocolUDP)),
	}
	in.Slices = []*discoveryv1.EndpointSlice{
		slice("games", "a", endpointSpec{addr: "10.244.5.5", node: "node-b", target: "pod-a"}),
	}
	st := Compute(in).State
	if len(st.Links) != 1 {
		t.Errorf("links = %d, want 1: %+v", len(st.Links), st.Links)
	}
	if len(st.Rules) != 2 {
		t.Errorf("rules = %d, want 2 (loop guard and divert): %+v", len(st.Rules), st.Rules)
	}
	if len(st.Routes) != 1 {
		t.Errorf("routes = %d, want 1: %+v", len(st.Routes), st.Routes)
	}
}
