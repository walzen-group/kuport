package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PortMapClassSpec is the admin-authored definition of a class: which nodes
// accept traffic, on which interfaces, which ports may be asked for, which
// namespaces may ask, and how return traffic gets home.
type PortMapClassSpec struct {
	// Nodes that accept traffic for this class. Every matching node gets rules.
	// +kubebuilder:validation:Required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// Interface names the DNAT rules match on, per accepting node. One rule per
	// interface per mapping.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Interfaces []string `json:"interfaces"`

	// +optional
	Ports *PortRange `json:"ports,omitempty"`

	// Which namespaces may reference this class. Empty selects every namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// +kubebuilder:validation:Required
	ReturnPath ReturnPath `json:"returnPath"`
}

// PortRange bounds the ports a PortMap in this class may ask for.
type PortRange struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=1
	// +optional
	Min int32 `json:"min,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=65535
	// +optional
	Max int32 `json:"max,omitempty"`

	// Ports the admin keeps back. A PortMap naming one is rejected in status.
	// +optional
	Reserved []int32 `json:"reserved,omitempty"`
}

// ReturnPathMode is how a reply from a pod on another node gets home.
// +kubebuilder:validation:Enum=Vxlan;None
type ReturnPathMode string

const (
	ReturnPathVxlan ReturnPathMode = "Vxlan"
	ReturnPathNone  ReturnPathMode = "None"
)

// ReturnPath configures how return traffic reaches the accepting node.
type ReturnPath struct {
	// None refuses any mapping whose pod is on another node.
	// +kubebuilder:validation:Required
	Mode ReturnPathMode `json:"mode"`

	// +optional
	Vxlan *VxlanConfig `json:"vxlan,omitempty"`
}

// VxlanConfig sets the parameters of the VXLAN return links this class builds.
type VxlanConfig struct {
	// +kubebuilder:default=4242
	// +optional
	VNI int32 `json:"vni,omitempty"`

	// Must differ from the CNI's, which is 8472 for Cilium.
	// +kubebuilder:default=4790
	// +optional
	Port int32 `json:"port,omitempty"`

	// Link addresses are allocated from here, a /31 per node pair.
	// +kubebuilder:default="169.254.77.0/24"
	// +optional
	Subnet string `json:"subnet,omitempty"`
}

// PortMapClassStatus records what each node's agent has done for this class and
// the return links allocated between node pairs.
type PortMapClassStatus struct {
	// One entry per node pair that has, or recently had, a return link.
	// +optional
	// +listType=map
	// +listMapKey=key
	Links []LinkAllocation `json:"links,omitempty"`

	// One row per node this class selects, maintained by that node's agent.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []NodeStatus `json:"nodes,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// LinkAllocation is the recorded-claim allocation for one node pair's return
// link, written by the agent holding a mapping's endpoint and read by the
// accepting node's agent.
type LinkAllocation struct {
	// The two node names joined by a slash, sorted. The list map key.
	// +kubebuilder:validation:Required
	Key string `json:"key"`

	// The two node names, sorted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	Peers []string `json:"peers"`

	// The allocated /31 out of the class subnet.
	// +kubebuilder:validation:Required
	Subnet string `json:"subnet"`

	// Zero-based index of this /31 within the class subnet. Fixes the routing
	// table id (200 + slot) and the packet mark (0x6b700000 | slot).
	// +kubebuilder:validation:Required
	Slot int32 `json:"slot"`

	// Set when no PortMap needs this pair. The entry is dropped once this is
	// more than 24h old. Absent while the pair is in use.
	// +optional
	UnusedSince *metav1.Time `json:"unusedSince,omitempty"`
}

// NodeStatus is one accepting node's readiness for this class, maintained by
// that node's agent.
type NodeStatus struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Ready bool `json:"ready"`

	// Why the node is not ready, when it is not.
	// +optional
	Message string `json:"message,omitempty"`

	// MTU of the interface carrying the return links, as read from the host.
	// +optional
	UnderlayMTU int32 `json:"underlayMTU,omitempty"`

	// UnderlayMTU minus the 50-byte VXLAN overhead. A reply larger than this
	// cannot cross a return link.
	// +optional
	LinkMTU int32 `json:"linkMTU,omitempty"`

	// Addresses of this node's interfaces, keyed by interface name. Each
	// accepting agent fills it for the class's interfaces it can resolve, so
	// the agent that writes a PortMap's status can fill the published rows
	// naming this node. An interface with no resolvable address is absent.
	// +optional
	Addresses map[string]string `json:"addresses,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=pmc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MODE",type=string,JSONPath=`.spec.returnPath.mode`
// +kubebuilder:printcolumn:name="INTERFACES",type=string,JSONPath=`.spec.interfaces`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`

// PortMapClass is the cluster-scoped, admin-authored definition of which nodes
// accept traffic for a set of mappings and how return traffic gets home.
type PortMapClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PortMapClassSpec   `json:"spec,omitempty"`
	Status PortMapClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PortMapClassList is a list of PortMapClass.
type PortMapClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PortMapClass `json:"items"`
}
