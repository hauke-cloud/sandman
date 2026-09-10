/*
Copyright 2026 hauke.cloud.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PowerState is the state an operator asks a machine to be in. It is the only
// part of the spec the REST API writes, which is what makes a power request
// survive a restart of sandman: the request is stored in the cluster, not in
// the process.
//
// The values are the two settled phases plus Unmanaged, so a spec and a status
// can be compared by reading them. They are deliberately not On and Off: YAML
// reads both of those as booleans, and `desiredPowerState: On` would be
// rejected by the API server as the boolean true -- a trap on the very first
// line anyone writes.
// +kubebuilder:validation:Enum=Running;Stopped;Unmanaged
type PowerState string

const (
	// PowerStateRunning asks for the machine to be running and its node
	// schedulable.
	PowerStateRunning PowerState = "Running"
	// PowerStateStopped asks for the machine to be shut down.
	PowerStateStopped PowerState = "Stopped"
	// PowerStateUnmanaged asks for nothing. The phase is still reported, so a
	// device can be observed before anything is allowed to act on it.
	PowerStateUnmanaged PowerState = "Unmanaged"
)

// PowerPhase is the observed state of a machine.
//
// Running and Stopped are settled; Starting, Stopping and Pending are in
// flight; Failed means the last transition gave up and no further action will
// be taken until the request changes.
// +kubebuilder:validation:Enum=Unknown;Pending;Starting;Running;Stopping;Stopped;Failed
type PowerPhase string

const (
	// PowerPhaseUnknown means the device has not been observed yet.
	PowerPhaseUnknown PowerPhase = "Unknown"
	// PowerPhasePending means a request has been accepted but the transition
	// has not begun -- typically the device is paused, or its node object has
	// not appeared yet.
	PowerPhasePending PowerPhase = "Pending"
	// PowerPhaseStarting means magic packets have been sent and sandman is
	// waiting for the node to register and become Ready.
	PowerPhaseStarting PowerPhase = "Starting"
	// PowerPhaseRunning means the node is registered and Ready. This is the
	// active state, and the one a workload cares about.
	PowerPhaseRunning PowerPhase = "Running"
	// PowerPhaseStopping means the node is being drained, or the shutdown pod
	// is running, or sandman is waiting for the node to go away.
	PowerPhaseStopping PowerPhase = "Stopping"
	// PowerPhaseStopped means the node is gone or has been unreachable since a
	// shutdown was issued.
	PowerPhaseStopped PowerPhase = "Stopped"
	// PowerPhaseFailed means the last transition timed out or errored.
	PowerPhaseFailed PowerPhase = "Failed"
)

// InFlight reports whether the phase is a transition rather than a resting
// state. Callers use it to decide whether to keep polling.
func (p PowerPhase) InFlight() bool {
	switch p {
	case PowerPhasePending, PowerPhaseStarting, PowerPhaseStopping:
		return true
	default:
		return false
	}
}

// Annotations and labels sandman sets on the objects it touches.
const (
	// AnnotationRequestID identifies one power request. The REST API writes a
	// fresh value on every call, and the controller records the one it acted
	// on in status. That is what lets an operator retry a request that failed
	// without editing the spec: a changed request id is a new request, an
	// unchanged one is the same request still failed.
	AnnotationRequestID = "sandman.hauke.cloud/request-id"
	// AnnotationRequestedBy records who asked, when the caller says.
	AnnotationRequestedBy = "sandman.hauke.cloud/requested-by"
	// AnnotationRequestedAt records when the request was accepted.
	AnnotationRequestedAt = "sandman.hauke.cloud/requested-at"
	// AnnotationRequestReason records why, when the caller says.
	AnnotationRequestReason = "sandman.hauke.cloud/request-reason"

	// LabelDevice and LabelNode are put on the pods sandman creates, so they
	// can be found -- and the informer cache narrowed to them -- without
	// guessing at names.
	LabelDevice = "sandman.hauke.cloud/device"
	LabelNode   = "sandman.hauke.cloud/node"
)

// Condition types set on a SandmanDevice.
const (
	// ConditionReady is true when the device's configuration resolves: the
	// node reference points at something and the MAC address is usable.
	ConditionReady = "Ready"
	// ConditionPowered is true when the machine is up, false when it is down,
	// and unknown while a transition is in flight.
	ConditionPowered = "Powered"
)

// SandmanDeviceSpec defines the desired state of a SandmanDevice.
type SandmanDeviceSpec struct {
	// NodeRef names the Kubernetes Node this machine registers as. It is the
	// link between the hardware described here and the workload scheduler's
	// view of it.
	// +kubebuilder:validation:Required
	NodeRef NodeReference `json:"nodeRef"`

	// DesiredPowerState is the state sandman drives the machine towards.
	// Unmanaged observes and reports without ever touching the hardware, which
	// is the safe default for a device that has just been added.
	// +kubebuilder:default=Unmanaged
	// +optional
	DesiredPowerState PowerState `json:"desiredPowerState,omitempty"`

	// WakeOnLAN describes how to start the machine.
	// +kubebuilder:validation:Required
	WakeOnLAN WakeOnLANSpec `json:"wakeOnLAN"`

	// Shutdown describes how to stop the machine.
	// +optional
	Shutdown ShutdownSpec `json:"shutdown,omitzero"`

	// Drain governs how workloads are moved off the node before it is stopped.
	// +optional
	Drain DrainSpec `json:"drain,omitzero"`

	// Timeouts bound how long a transition may take before it is declared
	// Failed.
	// +optional
	Timeouts TimeoutsSpec `json:"timeouts,omitzero"`

	// Paused stops sandman acting on this device without discarding the
	// desired state. A paused device reports Pending instead of transitioning,
	// which is the difference between "hold off" and "never mind".
	// +kubebuilder:default=false
	// +optional
	Paused bool `json:"paused,omitempty"`

	// Description is a free-form note about the machine, echoed by the REST
	// API. Useful for "the noisy one in the basement".
	// +optional
	Description string `json:"description,omitempty"`
}

// NodeReference points at a Kubernetes Node.
type NodeReference struct {
	// Name of the Node object. A stopped machine has no Node object at all, so
	// this is matched by name rather than resolved once and cached.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// WakeOnLANSpec describes the magic packet that starts the machine.
type WakeOnLANSpec struct {
	// MACAddress is the hardware address of the interface listening for magic
	// packets. Both `aa:bb:cc:dd:ee:ff` and `aa-bb-cc-dd-ee-ff` are accepted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$`
	MACAddress string `json:"macAddress"`

	// BroadcastAddress is where the packet is sent. The default is the
	// limited broadcast address, which only reaches the sender's own link --
	// so the sandman pod normally has to run with host networking. Set this to
	// a subnet's directed broadcast address (`192.168.1.255`) to reach another
	// segment, if the router in between forwards those.
	// +kubebuilder:default="255.255.255.255"
	// +optional
	BroadcastAddress string `json:"broadcastAddress,omitempty"`

	// Port is the UDP port the packet is sent to. Nothing listens on it -- the
	// NIC matches the payload, not the port -- but 9 and 7 are conventional
	// and firewalls tend to be written in terms of them.
	// +kubebuilder:default=9
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Packets is how many identical magic packets are sent per attempt. UDP
	// broadcast is unreliable and a sleeping NIC may miss one, so sending a
	// few costs nothing and is what everyone does.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +optional
	Packets int32 `json:"packets,omitempty"`

	// PacketInterval is the gap between those packets.
	// +kubebuilder:default="200ms"
	// +optional
	PacketInterval *metav1.Duration `json:"packetInterval,omitempty"`

	// SecureOnSecretRef points at a Secret holding a SecureOn password: six
	// bytes appended to the magic packet, which some NICs require. The value
	// may be written as hex (`aa:bb:cc:dd:ee:ff`) or as six raw bytes.
	// +optional
	SecureOnSecretRef *SecretKeyReference `json:"secureOnSecretRef,omitempty"`
}

// SecretKeyReference selects a key of a Secret. It carries an explicit
// namespace because SandmanDevice is cluster-scoped.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Namespace of the Secret. Defaults to the namespace sandman runs in.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Key inside the Secret.
	// +kubebuilder:default=password
	// +optional
	Key string `json:"key,omitempty"`
}

// ShutdownMode selects how a machine is powered off.
// +kubebuilder:validation:Enum=PrivilegedPod;Disabled
type ShutdownMode string

const (
	// ShutdownModePrivilegedPod powers the machine off by scheduling a
	// privileged pod onto it that enters the host's namespaces and calls
	// systemctl poweroff.
	ShutdownModePrivilegedPod ShutdownMode = "PrivilegedPod"
	// ShutdownModeDisabled refuses to stop the machine. Requests to stop it
	// fail loudly rather than being ignored.
	ShutdownModeDisabled ShutdownMode = "Disabled"
)

// ShutdownSpec describes how the machine is powered off.
//
// There is no out-of-band path here on purpose: no IPMI, no BMC, no serial
// console. The one thing sandman can always reach on a running node is the
// kubelet, so the node powers itself off from the inside.
type ShutdownSpec struct {
	// Mode selects the shutdown mechanism.
	// +kubebuilder:default=PrivilegedPod
	// +optional
	Mode ShutdownMode `json:"mode,omitempty"`

	// Image is the container image the shutdown pod runs. It needs a shell and
	// nsenter, both of which busybox has.
	// +kubebuilder:default="busybox:1.37"
	// +optional
	Image string `json:"image,omitempty"`

	// Command overrides the shutdown command. The default enters PID 1's mount
	// and PID namespaces and asks systemd to power off, which runs the host's
	// own shutdown sequence -- unmounting filesystems and stopping services --
	// rather than cutting power under a running kernel.
	// +optional
	Command []string `json:"command,omitempty"`

	// Namespace is where the shutdown pod is created. Empty means the
	// namespace sandman runs in. It must permit privileged pods, so it is
	// worth keeping separate from anything else.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Tolerations are added to the shutdown pod. The defaults let it land on a
	// node that is already cordoned, unreachable or not ready -- which is
	// exactly the node it has to land on.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// PriorityClassName is the priority class of the shutdown pod. A busy node
	// can be short of resources exactly when it needs to be shut down, and a
	// preempted poweroff pod means a machine that never turns off. Leave it
	// empty unless the cluster's admission rules allow the class here: the
	// system-node-critical class is restricted to kube-system on most
	// clusters.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// ActiveDeadlineSeconds bounds the shutdown pod's life. The pod is
	// expected to die with its node, so this only matters when the poweroff
	// silently does nothing.
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=10
	// +optional
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`
}

// DrainSpec governs how workloads leave the node before it stops.
type DrainSpec struct {
	// Enabled evicts pods before the machine is stopped. Turning this off
	// stops the node with its workloads still on it; they are rescheduled once
	// the node's grace period expires, which is much slower and much ruder.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Cordon marks the node unschedulable before stopping it. This happens
	// even when Enabled is false, because scheduling onto a machine that is
	// about to lose power is never right.
	// +kubebuilder:default=true
	// +optional
	Cordon *bool `json:"cordon,omitempty"`

	// UncordonOnStart makes the node schedulable again once it comes back.
	// Without it a machine started by sandman would come up idle.
	// +kubebuilder:default=true
	// +optional
	UncordonOnStart *bool `json:"uncordonOnStart,omitempty"`

	// IgnoreDaemonSets proceeds even though DaemonSet pods cannot be evicted.
	// They are recreated on the next node, so waiting for them never ends.
	// +kubebuilder:default=true
	// +optional
	IgnoreDaemonSets *bool `json:"ignoreDaemonSets,omitempty"`

	// DeleteEmptyDirData evicts pods with emptyDir volumes, discarding their
	// contents. Without it a single scratch volume blocks the whole drain.
	// +kubebuilder:default=true
	// +optional
	DeleteEmptyDirData *bool `json:"deleteEmptyDirData,omitempty"`

	// Force deletes pods that no controller will recreate. They are lost, but
	// so they would be when the power goes.
	// +kubebuilder:default=false
	// +optional
	Force *bool `json:"force,omitempty"`

	// GracePeriodSeconds overrides each pod's terminationGracePeriodSeconds.
	// Negative means each pod keeps its own.
	// +kubebuilder:default=-1
	// +optional
	GracePeriodSeconds int64 `json:"gracePeriodSeconds,omitempty"`

	// Timeout bounds the drain. When it expires the stop is abandoned rather
	// than forced: a drain that will not finish usually means a PodDisruptionBudget
	// is being honoured, and powering the machine off anyway would violate it.
	// +kubebuilder:default="5m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// TimeoutsSpec bounds the two transitions.
type TimeoutsSpec struct {
	// Start is how long a machine has to register and become Ready after the
	// first magic packet before the start is declared Failed. It has to cover
	// POST, boot and the kubelet's first successful heartbeat.
	// +kubebuilder:default="10m"
	// +optional
	Start *metav1.Duration `json:"start,omitempty"`

	// Stop is how long the whole stop may take: drain, shutdown pod and the
	// node going away.
	// +kubebuilder:default="10m"
	// +optional
	Stop *metav1.Duration `json:"stop,omitempty"`

	// WakeRetryInterval is how often the magic packet is resent while a start
	// is in flight. A packet sent while the NIC is still settling after a
	// previous shutdown is simply dropped, so repeating is what makes a start
	// reliable.
	// +kubebuilder:default="1m"
	// +optional
	WakeRetryInterval *metav1.Duration `json:"wakeRetryInterval,omitempty"`

	// StoppedGracePeriod is how long the node has to disappear or go
	// unreachable after the shutdown command before sandman concludes the
	// machine is off. A node's Ready condition goes stale rather than false
	// when the kubelet dies with the machine, so this is the delay before an
	// unreachable node counts as stopped.
	// +kubebuilder:default="2m"
	// +optional
	StoppedGracePeriod *metav1.Duration `json:"stoppedGracePeriod,omitempty"`
}

// SandmanDeviceStatus is the observed state of a SandmanDevice.
type SandmanDeviceStatus struct {
	// Phase is the machine's observed power state.
	// +kubebuilder:default=Unknown
	// +optional
	Phase PowerPhase `json:"phase,omitempty"`

	// Reason is a machine-readable reason for the current phase.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message describes the current phase in human terms.
	// +optional
	Message string `json:"message,omitempty"`

	// NodeFound reports whether the referenced Node object exists. A machine
	// that is off usually has none.
	// +optional
	NodeFound bool `json:"nodeFound,omitempty"`

	// NodeReady mirrors the Node's Ready condition. Nil when there is no node.
	// +optional
	NodeReady *bool `json:"nodeReady,omitempty"`

	// Unschedulable mirrors the Node's cordon state.
	// +optional
	Unschedulable *bool `json:"unschedulable,omitempty"`

	// Address is an address of the node, taken from its status. It is recorded
	// while the node is up so that a later start can aim a directed broadcast
	// at the right subnet.
	// +optional
	Address string `json:"address,omitempty"`

	// PhaseSince is when the current phase was entered. Transitions are
	// declared failed relative to this.
	// +optional
	PhaseSince *metav1.Time `json:"phaseSince,omitempty"`

	// LastWakeTime is when the last magic packet was sent.
	// +optional
	LastWakeTime *metav1.Time `json:"lastWakeTime,omitempty"`

	// WakeAttempts counts the magic packets sent during the transition in
	// flight. It resets when a start finishes.
	// +optional
	WakeAttempts int32 `json:"wakeAttempts,omitempty"`

	// LastShutdownTime is when the shutdown command was last issued.
	// +optional
	LastShutdownTime *metav1.Time `json:"lastShutdownTime,omitempty"`

	// LastStartedTime and LastStoppedTime are when the machine last settled
	// into each state.
	// +optional
	LastStartedTime *metav1.Time `json:"lastStartedTime,omitempty"`
	// +optional
	LastStoppedTime *metav1.Time `json:"lastStoppedTime,omitempty"`

	// ShutdownPod names the privileged pod carrying out the current stop.
	// +optional
	ShutdownPod string `json:"shutdownPod,omitempty"`

	// LastRequestID is the request id the controller last acted on. A device
	// whose annotation carries a different id has an unhandled request, even
	// when its desired state is unchanged.
	// +optional
	LastRequestID string `json:"lastRequestID,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the current state of the SandmanDevice.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sd;sandman
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeRef.name`
// +kubebuilder:printcolumn:name="MAC",type=string,JSONPath=`.spec.wakeOnLAN.macAddress`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredPowerState`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.nodeReady`
// +kubebuilder:printcolumn:name="Since",type=date,JSONPath=`.status.phaseSince`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SandmanDevice is the Schema for the sandmandevices API. It describes one
// physical machine that backs a Kubernetes node, and how to turn it on and off.
//
// It is cluster-scoped because a Node is, and because two SandmanDevices in
// different namespaces pointing at one machine would be two controllers
// fighting over a power switch.
type SandmanDevice struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SandmanDevice
	// +required
	Spec SandmanDeviceSpec `json:"spec"`

	// status defines the observed state of SandmanDevice
	// +optional
	Status SandmanDeviceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SandmanDeviceList contains a list of SandmanDevice.
type SandmanDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SandmanDevice `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandmanDevice{}, &SandmanDeviceList{})
}
