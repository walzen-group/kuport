package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Class kinds a Link can belong to.
const (
	ClassKindPortMapClass = "PortMapClass"
	ClassKindEgressClass  = "EgressClass"
)

// LinkSpec names the class and the two nodes a return link connects. The slot
// is the object's name, slot-<n>, so the API server refuses a second claim on
// the same slot with AlreadyExists, cluster-wide and across every class.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type LinkSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=PortMapClass;EgressClass
	ClassKind string `json:"classKind"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClassName string `json:"className"`

	// The two node names, sorted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	Peers []string `json:"peers"`
}

// LinkStatus records whether anything still needs the link.
type LinkStatus struct {
	// Set when no mapping or Egress needs this link. Any agent deletes the Link
	// once this is more than 24h old. Absent while the link is in use.
	// +optional
	UnusedSince *metav1.Time `json:"unusedSince,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="KIND",type=string,JSONPath=`.spec.classKind`
// +kubebuilder:printcolumn:name="CLASS",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="PEERS",type=string,JSONPath=`.spec.peers`
// +kubebuilder:printcolumn:name="UNUSED-SINCE",type=date,JSONPath=`.status.unusedSince`

// Link is one return link between two nodes, claimed from the cluster-wide pool.
// Its name, slot-<n>, fixes the link's VNI, addresses, packet mark, routing
// table and device name.
type Link struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LinkSpec   `json:"spec,omitempty"`
	Status LinkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LinkList is a list of Link.
type LinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Link `json:"items"`
}
