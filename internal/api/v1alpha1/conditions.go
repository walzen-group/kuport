package v1alpha1

// Condition types and reasons, exported so the reconcile (task 3) and the agent
// (task 6) set them from constants rather than retyping the strings.

// PortMap condition types.
const (
	// ConditionAccepted reports whether the mapping is admissible: its class
	// exists, selects the namespace, and its port is in range, unreserved, and
	// unclaimed by an earlier mapping.
	ConditionAccepted = "Accepted"

	// ConditionProgrammed reports whether the serving node has written its
	// rules for the mapping.
	ConditionProgrammed = "Programmed"
)

// PortMapClass condition types.
const (
	// ConditionReady reports whether the class is usable.
	ConditionReady = "Ready"
)

// Shared True reason for Accepted and Ready.
const (
	ReasonValid = "Valid"
)

// PortMap Accepted=False reasons.
const (
	ReasonClassNotFound        = "ClassNotFound"
	ReasonNamespaceNotSelected = "NamespaceNotSelected"
	ReasonPortOutOfRange       = "PortOutOfRange"
	ReasonPortReserved         = "PortReserved"
	ReasonPortConflict         = "PortConflict"
	ReasonInvalidPortRange     = "InvalidPortRange"
	ReasonInterfaceNotInClass  = "InterfaceNotInClass"
)

// PortMap Programmed reasons.
const (
	ReasonAllNodesReady         = "AllNodesReady"
	ReasonNoReadyEndpoint       = "NoReadyEndpoint"
	ReasonReturnPathUnavailable = "ReturnPathUnavailable"
	ReasonNodeNotReady          = "NodeNotReady"
)

// PortMapClass Ready=False reasons.
const (
	ReasonTunnelModeRequired = "TunnelModeRequired"
	ReasonVxlanPortConflict  = "VxlanPortConflict"
	ReasonSubnetExhausted    = "SubnetExhausted"
	ReasonInvalidSubnet      = "InvalidSubnet"
)
