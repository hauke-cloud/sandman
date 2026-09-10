package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
)

// listDevices answers with every device and a count per state.
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.list(r.Context())
	if err != nil {
		s.fail(w, err, "listing devices failed")
		return
	}

	list := DeviceList{
		Items:   make([]Device, 0, len(devices)),
		Summary: map[v1alpha1.PowerPhase]int{},
	}
	for i := range devices {
		view := newDevice(&devices[i])
		list.Items = append(list.Items, view)
		list.Summary[view.State]++
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	list.Count = len(list.Items)

	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := s.deviceByName(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newDevice(device))
}

// getDeviceStatus answers with the state alone. It exists so that a poller --
// a script waiting for a machine to come up, a status light on a wall -- can
// ask a question whose answer is three fields rather than thirty.
func (s *Server) getDeviceStatus(w http.ResponseWriter, r *http.Request) {
	device, ok := s.deviceByName(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	view := newDevice(device)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         view.Name,
		"node":         view.Node,
		"state":        view.State,
		"desiredState": view.DesiredState,
		"inFlight":     view.InFlight,
		"reason":       view.Reason,
		"message":      view.Message,
		"since":        view.Timestamps.PhaseSince,
	})
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	device, ok := s.deviceByNode(w, r, r.PathValue("node"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newDevice(device))
}

func (s *Server) getNodeStatus(w http.ResponseWriter, r *http.Request) {
	device, ok := s.deviceByNode(w, r, r.PathValue("node"))
	if !ok {
		return
	}
	r.SetPathValue("name", device.Name)
	s.getDeviceStatus(w, r)
}

func (s *Server) startDevice(w http.ResponseWriter, r *http.Request) {
	s.requestPower(w, r, r.PathValue("name"), "", v1alpha1.PowerStateRunning)
}

func (s *Server) stopDevice(w http.ResponseWriter, r *http.Request) {
	s.requestPower(w, r, r.PathValue("name"), "", v1alpha1.PowerStateStopped)
}

// releaseDevice hands a machine back: sandman keeps reporting its state and
// stops acting on it. It is the way out of a stop that is not a start.
func (s *Server) releaseDevice(w http.ResponseWriter, r *http.Request) {
	s.requestPower(w, r, r.PathValue("name"), "", v1alpha1.PowerStateUnmanaged)
}

func (s *Server) startNode(w http.ResponseWriter, r *http.Request) {
	s.requestPower(w, r, "", r.PathValue("node"), v1alpha1.PowerStateRunning)
}

func (s *Server) stopNode(w http.ResponseWriter, r *http.Request) {
	s.requestPower(w, r, "", r.PathValue("node"), v1alpha1.PowerStateStopped)
}

// requestPower records a desired power state on the device and returns. The
// controller does the rest.
func (s *Server) requestPower(w http.ResponseWriter, r *http.Request, name, node string, desired v1alpha1.PowerState) {
	if s.opts.ReadOnly {
		writeError(w, http.StatusForbidden, "read_only",
			"this sandman is running read-only and will not change a machine's power state", "")
		return
	}

	var body PowerRequest
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request body is not valid JSON", err.Error())
		return
	}

	var (
		device *v1alpha1.SandmanDevice
		ok     bool
	)
	if name != "" {
		device, ok = s.deviceByName(w, r, name)
	} else {
		device, ok = s.deviceByNode(w, r, node)
	}
	if !ok {
		return
	}

	if desired == v1alpha1.PowerStateStopped &&
		device.Spec.Shutdown.ShutdownMode() == v1alpha1.ShutdownModeDisabled {
		// Refusing here rather than writing a request the controller would
		// immediately fail keeps the answer where the caller is looking.
		writeError(w, http.StatusConflict, "shutdown_disabled",
			fmt.Sprintf("device %s has spec.shutdown.mode=Disabled", device.Name), "")
		return
	}

	requestID := uuid.NewString()
	patch := client.MergeFrom(device.DeepCopy())
	device.Spec.DesiredPowerState = desired
	annotate(device, map[string]string{
		v1alpha1.AnnotationRequestID:     requestID,
		v1alpha1.AnnotationRequestedAt:   time.Now().UTC().Format(time.RFC3339),
		v1alpha1.AnnotationRequestedBy:   body.RequestedBy,
		v1alpha1.AnnotationRequestReason: body.Reason,
	})

	if err := s.opts.Client.Patch(r.Context(), device, patch); err != nil {
		s.fail(w, err, "recording the power request failed")
		return
	}

	s.logger.Info("power request accepted",
		"device", device.Name,
		"node", device.Spec.NodeRef.Name,
		"desired", desired,
		"request_id", requestID,
		"requested_by", body.RequestedBy,
		"reason", body.Reason)

	writeJSON(w, http.StatusAccepted, PowerResponse{
		Accepted:  true,
		RequestID: requestID,
		Device:    newDevice(device),
		Message:   message(desired, device),
	})
}

// message says what was recorded, in the terms the caller asked in.
func message(desired v1alpha1.PowerState, device *v1alpha1.SandmanDevice) string {
	switch desired {
	case v1alpha1.PowerStateRunning:
		if device.Spec.Paused {
			return fmt.Sprintf("recorded, but %s is paused: no magic packet will be sent until it is resumed", device.Name)
		}
		return fmt.Sprintf("waking %s; poll its state until it reports Started", device.Spec.NodeRef.Name)
	case v1alpha1.PowerStateStopped:
		if device.Spec.Paused {
			return fmt.Sprintf("recorded, but %s is paused: the machine will not be stopped until it is resumed", device.Name)
		}
		if device.Spec.Drain.DrainEnabled() {
			return fmt.Sprintf("stopping %s; it will be cordoned and drained before it powers off", device.Spec.NodeRef.Name)
		}
		return fmt.Sprintf("stopping %s without draining it first", device.Spec.NodeRef.Name)
	default:
		return device.Name + " is no longer managed; its state is still reported"
	}
}

func annotate(device *v1alpha1.SandmanDevice, annotations map[string]string) {
	if device.Annotations == nil {
		device.Annotations = map[string]string{}
	}
	for key, value := range annotations {
		if value == "" {
			// An omitted field clears whatever the previous request left, so
			// that the annotations describe this request and not a mixture of
			// this one and the last.
			delete(device.Annotations, key)
			continue
		}
		device.Annotations[key] = value
	}
}

// list reads every device from the cache.
func (s *Server) list(ctx context.Context) ([]v1alpha1.SandmanDevice, error) {
	var devices v1alpha1.SandmanDeviceList
	if err := s.opts.Client.List(ctx, &devices); err != nil {
		return nil, err
	}
	return devices.Items, nil
}

func (s *Server) deviceByName(w http.ResponseWriter, r *http.Request, name string) (*v1alpha1.SandmanDevice, bool) {
	var device v1alpha1.SandmanDevice
	if err := s.opts.Client.Get(r.Context(), types.NamespacedName{Name: name}, &device); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("no SandmanDevice named %q", name), "")
			return nil, false
		}
		s.fail(w, err, "reading the device failed")
		return nil, false
	}
	return &device, true
}

// deviceByNode finds the device managing a node. More than one device pointing
// at the same node is a misconfiguration rather than a choice to make, so it is
// reported instead of being resolved arbitrarily.
func (s *Server) deviceByNode(w http.ResponseWriter, r *http.Request, node string) (*v1alpha1.SandmanDevice, bool) {
	devices, err := s.list(r.Context())
	if err != nil {
		s.fail(w, err, "listing devices failed")
		return nil, false
	}

	var matches []*v1alpha1.SandmanDevice
	for i := range devices {
		if devices[i].Spec.NodeRef.Name == node {
			matches = append(matches, &devices[i])
		}
	}

	switch len(matches) {
	case 0:
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no SandmanDevice manages node %q", node), "")
		return nil, false
	case 1:
		return matches[0], true
	default:
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Name)
		}
		sort.Strings(names)
		writeError(w, http.StatusConflict, "ambiguous_node",
			fmt.Sprintf("%d devices manage node %q", len(matches), node),
			fmt.Sprintf("address one of them directly: %v", names))
		return nil, false
	}
}

// fail turns a Kubernetes API error into an HTTP one, keeping the status the
// API server chose where there is a sensible equivalent.
func (s *Server) fail(w http.ResponseWriter, err error, message string) {
	s.logger.Error(message, "error", err)

	switch {
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, "not_found", message, err.Error())
	case apierrors.IsConflict(err):
		writeError(w, http.StatusConflict, "conflict",
			"the device changed while the request was being written; try again", err.Error())
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		writeError(w, http.StatusForbidden, "forbidden",
			"sandman is not allowed to do this in the cluster", err.Error())
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err):
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"the Kubernetes API is not answering right now", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", message, err.Error())
	}
}
