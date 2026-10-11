package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EgressClassSpec names the node selected pods' outbound traffic leaves the
// cluster from, and which namespaces may use it.
type EgressClassSpec struct {
	// The egress node. Changing it moves every Egress of the class on the next
	// pass.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`

	// Which namespaces may reference this class. Empty selects every namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// EgressClassStatus holds a row per node taking part: the egress node and every
// node holding a selected pod. Each node's agent writes its own row.
type EgressClassStatus struct {
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

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=egc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="NODE",type=string,JSONPath=`.spec.node`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`

// EgressClass is the cluster-scoped, admin-authored choice of the node that
// selected pods' traffic toward the default route leaves the cluster from.
type EgressClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressClassSpec   `json:"spec,omitempty"`
	Status EgressClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EgressClassList is a list of EgressClass.
type EgressClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressClass `json:"items"`
}

// EgressSpec selects pods whose connections toward the default route leave the
// cluster from an EgressClass's node.
//
// +kubebuilder:validation:XValidation:rule="self.className == oldSelf.className",message="className is immutable"
type EgressSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClassName string `json:"className"`

	// Pods in the Egress's namespace. An empty selector would select every pod
	// in the namespace and is refused.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="podSelector must select something"
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// True drops a selected pod's traffic toward the default route while its
	// path through the egress node is down. False lets that traffic leave from
	// the pod's own node meanwhile. Traffic with a more specific route keeps it
	// either way.
	// +optional
	FailClosed bool `json:"failClosed,omitempty"`
}

// EgressPath is how a selected pod's traffic toward the default route leaves.
// +kubebuilder:validation:Enum=Link;Local;Blocked;Direct
type EgressPath string

const (
	// EgressPathLink: over a link to the egress node, which masquerades it.
	EgressPathLink EgressPath = "Link"
	// EgressPathLocal: the pod runs on the egress node and leaves from it.
	EgressPathLocal EgressPath = "Local"
	// EgressPathBlocked: the path is down and the Egress fails closed.
	EgressPathBlocked EgressPath = "Blocked"
	// EgressPathDirect: the path is down and the pod leaves from its own node.
	EgressPathDirect EgressPath = "Direct"
)

// EgressPod is one selected pod and the path its node programmed for it.
type EgressPod struct {
	Name    string     `json:"name"`
	Node    string     `json:"node"`
	Address string     `json:"address"`
	Path    EgressPath `json:"path"`

	// +optional
	Address6 string `json:"address6,omitempty"`
	// +optional
	Path6 EgressPath `json:"path6,omitempty"`
}

// EgressStatus reports the egress node, every selected pod's path and the
// Egress's conditions.
type EgressStatus struct {
	// +optional
	Node string `json:"node,omitempty"`

	// +optional
	Pods []EgressPod `json:"pods,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=eg
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CLASS",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="NODE",type=string,JSONPath=`.status.node`
// +kubebuilder:printcolumn:name="FAIL-CLOSED",type=boolean,JSONPath=`.spec.failClosed`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`

// Egress is a namespaced request that the selected pods' traffic toward the
// default route leave the cluster from an EgressClass's node.
type Egress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressSpec   `json:"spec,omitempty"`
	Status EgressStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EgressList is a list of Egress.
type EgressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Egress `json:"items"`
}
