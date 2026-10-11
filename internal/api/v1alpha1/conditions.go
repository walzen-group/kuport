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

	// ConditionDeprecated is True when the class sets a field kuport ignores.
	// It leaves Ready alone: the class works, the field does nothing.
	ConditionDeprecated = "Deprecated"
)

// PortMapClass Deprecated reasons.
const (
	ReasonDeprecatedField = "DeprecatedField"
	ReasonNoDeprecated    = "NoDeprecatedFields"
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
	// ReasonRangeTranslation: a range whose named port lists another number.
	ReasonRangeTranslation = "RangeTranslation"
	// ReasonTargetPortInUse: a translating mapping shares its Service port.
	ReasonTargetPortInUse = "TargetPortInUse"
)

// PortMap Programmed reasons.
const (
	ReasonAllNodesReady         = "AllNodesReady"
	ReasonNoReadyEndpoint       = "NoReadyEndpoint"
	ReasonReturnPathUnavailable = "ReturnPathUnavailable"
	ReasonNodeNotReady          = "NodeNotReady"
	ReasonNoInterfaceOnNode     = "NoInterfaceOnNode"
)

// PortMapClass Ready=False reasons.
const (
	ReasonTunnelModeRequired = "TunnelModeRequired"
	ReasonVxlanPortConflict  = "VxlanPortConflict"
	// ReasonSubnetExhausted: every slot of the cluster-wide link pool is taken.
	ReasonSubnetExhausted = "SubnetExhausted"
)

// Egress reasons. Accepted reuses ReasonValid, ReasonClassNotFound and
// ReasonNamespaceNotSelected; Programmed reuses ReasonReturnPathUnavailable
// and ReasonSubnetExhausted.
const (
	// ReasonAllPodsRouted: every selected pod's path is Link or Local.
	ReasonAllPodsRouted = "AllPodsRouted"
	// ReasonEgressConflict: a selected pod is selected by an earlier Egress,
	// which decides its path.
	ReasonEgressConflict = "EgressConflict"
	// ReasonPodCIDRsUnknown: a Node has no spec.podCIDRs, so the cluster's
	// own destinations cannot be told apart and every path is down.
	ReasonPodCIDRsUnknown = "PodCIDRsUnknown"
	// ReasonEgressNodeNotFound: the class's node names no Node.
	ReasonEgressNodeNotFound = "EgressNodeNotFound"
)
