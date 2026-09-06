package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PortMapSpec is what a workload asks for: a port on an accepting node of a
// class, delivered to a pod behind a Service in the same namespace.
//
// The immutable fields are immutable because changing one is indistinguishable
// from deleting a mapping and creating another, and the reconcile is simpler if
// it never has to unwind a half-changed mapping.
//
// +kubebuilder:validation:XValidation:rule="self.className == oldSelf.className",message="className is immutable"
// +kubebuilder:validation:XValidation:rule="self.protocol == oldSelf.protocol",message="protocol is immutable"
// +kubebuilder:validation:XValidation:rule="self.port == oldSelf.port",message="port is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.endPort) == has(oldSelf.endPort) && (!has(self.endPort) || self.endPort == oldSelf.endPort)",message="endPort is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.endPort) || self.endPort >= self.port",message="endPort must be greater than or equal to port"
type PortMapSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClassName string `json:"className"`

	// +kubebuilder:validation:Required
	Protocol Protocol `json:"protocol"`

	// The port clients dial on an accepting node, and the port the pod
	// receives on. With EndPort set, the first port of an inclusive range.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Last port of an inclusive range starting at Port. Omit for a single port.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	EndPort *int32 `json:"endPort,omitempty"`

	// +kubebuilder:validation:Required
	ServiceRef ServiceRef `json:"serviceRef"`
}

// Protocol is the L4 protocol a mapping carries.
// +kubebuilder:validation:Enum=TCP;UDP
type Protocol string

const (
	ProtocolTCP Protocol = "TCP"
	ProtocolUDP Protocol = "UDP"
)

// ServiceRef names the Service whose endpoints the agent follows.
type ServiceRef struct {
	// A Service in the same namespace. Any type, ClusterIP included. It exists
	// so the agent has endpoints to follow and a named port to resolve.
	// kuport never modifies it.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// The named port on that Service.
	// +kubebuilder:validation:Required
	Port string `json:"port"`
}

// PortMapStatus reports the chosen endpoint, the addresses the port answers on
// right now, and the mapping's conditions.
type PortMapStatus struct {
	// +optional
	Endpoint *Endpoint `json:"endpoint,omitempty"`

	// The addresses this port answers on right now, one row per class
	// interface on the mapping's serving node, the node where the DNAT rules
	// exist. This is what a person needs from `kubectl get portmap`.
	// +optional
	Published []PublishedAddress `json:"published,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Endpoint is the pod the mapping currently delivers to.
type Endpoint struct {
	Node    string `json:"node"`
	Address string `json:"address"`
}

// PublishedAddress is one address the port answers on, on one accepting node
// and interface.
type PublishedAddress struct {
	Node      string `json:"node"`
	Interface string `json:"interface"`
	Address   string `json:"address"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=pm
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CLASS",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="PROTO",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="PORT",type=integer,JSONPath=`.spec.port`
// +kubebuilder:printcolumn:name="ENDPOINT",type=string,JSONPath=`.status.endpoint.address`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="PUBLISHED",type=string,priority=1,JSONPath=`.status.published[*].address`

// PortMap is a namespaced request to deliver a port on an accepting node to a
// pod behind a Service in the same namespace.
type PortMap struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PortMapSpec   `json:"spec,omitempty"`
	Status PortMapStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PortMapList is a list of PortMap.
type PortMapList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PortMap `json:"items"`
}
