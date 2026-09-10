// Package server serves sandman's REST API, its Prometheus metrics and the two
// probes Kubernetes needs.
//
// The API is deliberately thin. Reads come straight from the controller's
// informer cache, and a write does one thing: it records the requested power
// state on the SandmanDevice and returns. The controller notices and acts. That
// means a request survives sandman restarting, a caller never holds a
// connection open across a machine's boot, and two replicas cannot fight --
// only the leader acts, whichever one took the call.
package server

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
)

// Device is the API's view of a SandmanDevice.
type Device struct {
	// Name of the SandmanDevice.
	Name string `json:"name"`
	// Node is the Kubernetes node the machine registers as.
	Node string `json:"node"`
	// Description is the operator's note about the machine.
	Description string `json:"description,omitempty"`
	// MACAddress is the address magic packets are sent to.
	MACAddress string `json:"macAddress"`
	// BroadcastAddress is where those packets go.
	BroadcastAddress string `json:"broadcastAddress"`
	// Address is the node's last known address on the network.
	Address string `json:"address,omitempty"`

	// State is the observed power state: Unknown, Pending, Starting, Started,
	// Stopping, Stopped or Failed.
	State v1alpha1.PowerPhase `json:"state"`
	// DesiredState is what sandman has been asked for: On, Off or Unmanaged.
	DesiredState v1alpha1.PowerState `json:"desiredState"`
	// InFlight is true while the machine is between states, which is the
	// signal for a caller to poll rather than to conclude.
	InFlight bool `json:"inFlight"`
	// Reason is a short machine-readable reason for the state.
	Reason string `json:"reason,omitempty"`
	// Message describes the state in human terms.
	Message string `json:"message,omitempty"`
	// Paused reports whether sandman is holding off acting on this device.
	Paused bool `json:"paused"`

	// NodeStatus is what the cluster currently sees of the node.
	NodeStatus NodeStatus `json:"nodeStatus"`
	// Timestamps records when things last happened.
	Timestamps Timestamps `json:"timestamps"`
	// WakeAttempts counts magic packets sent during the start in flight.
	WakeAttempts int32 `json:"wakeAttempts,omitempty"`
	// ShutdownPod names the privileged pod carrying out a stop.
	ShutdownPod string `json:"shutdownPod,omitempty"`
	// Conditions are the device's Kubernetes conditions, passed through.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NodeStatus is the cluster's view of the machine.
type NodeStatus struct {
	// Found reports whether a Node object exists. A stopped machine has none.
	Found bool `json:"found"`
	// Ready mirrors the node's Ready condition. Nil when there is no node.
	Ready *bool `json:"ready,omitempty"`
	// Unschedulable mirrors the cordon.
	Unschedulable *bool `json:"unschedulable,omitempty"`
}

// Timestamps records when the device last did each thing.
type Timestamps struct {
	PhaseSince   *time.Time `json:"phaseSince,omitempty"`
	LastWake     *time.Time `json:"lastWake,omitempty"`
	LastShutdown *time.Time `json:"lastShutdown,omitempty"`
	LastStarted  *time.Time `json:"lastStarted,omitempty"`
	LastStopped  *time.Time `json:"lastStopped,omitempty"`
}

// DeviceList is the response of the collection endpoints.
type DeviceList struct {
	// Items are the devices, ordered by name.
	Items []Device `json:"items"`
	// Count is len(Items), so a caller can check a truncated read.
	Count int `json:"count"`
	// Summary counts devices by state, which is the whole fleet in one line.
	Summary map[v1alpha1.PowerPhase]int `json:"summary"`
}

// PowerRequest is the optional body of a start or stop call. Every field is
// for the record only: none of them change what sandman does, they change what
// it can tell you afterwards.
type PowerRequest struct {
	// RequestedBy names the person or system asking.
	RequestedBy string `json:"requestedBy,omitempty"`
	// Reason says why.
	Reason string `json:"reason,omitempty"`
}

// PowerResponse is returned by a start or stop call.
type PowerResponse struct {
	// Accepted is true when the request was recorded. It says nothing about
	// the machine yet: a start is accepted long before it boots.
	Accepted bool `json:"accepted"`
	// RequestID identifies this request. It comes back in the device's
	// annotations and, once acted on, in its status.
	RequestID string `json:"requestID"`
	// Device is the device as it stands immediately after the request.
	Device Device `json:"device"`
	// Message describes what was recorded.
	Message string `json:"message"`
}

// Error is the body of every failed request.
type Error struct {
	// Error is a short machine-readable code.
	Error string `json:"error"`
	// Message explains it.
	Message string `json:"message"`
	// Details carries anything specific to the failure.
	Details string `json:"details,omitempty"`
}

// newDevice projects a SandmanDevice onto the API's view of it.
func newDevice(device *v1alpha1.SandmanDevice) Device {
	status := &device.Status

	phase := status.Phase
	if phase == "" {
		phase = v1alpha1.PowerPhaseUnknown
	}

	return Device{
		Name:             device.Name,
		Node:             device.Spec.NodeRef.Name,
		Description:      device.Spec.Description,
		MACAddress:       device.Spec.WakeOnLAN.MACAddress,
		BroadcastAddress: device.Spec.WakeOnLAN.BroadcastAddr(),
		Address:          status.Address,
		State:            phase,
		DesiredState:     device.Spec.Desired(),
		InFlight:         phase.InFlight(),
		Reason:           status.Reason,
		Message:          status.Message,
		Paused:           device.Spec.Paused,
		NodeStatus: NodeStatus{
			Found:         status.NodeFound,
			Ready:         status.NodeReady,
			Unschedulable: status.Unschedulable,
		},
		Timestamps: Timestamps{
			PhaseSince:   plainTime(status.PhaseSince),
			LastWake:     plainTime(status.LastWakeTime),
			LastShutdown: plainTime(status.LastShutdownTime),
			LastStarted:  plainTime(status.LastStartedTime),
			LastStopped:  plainTime(status.LastStoppedTime),
		},
		WakeAttempts: status.WakeAttempts,
		ShutdownPod:  status.ShutdownPod,
		Conditions:   status.Conditions,
	}
}

func plainTime(t *metav1.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	out := t.UTC()
	return &out
}
