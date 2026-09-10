package power

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
	"github.com/hauke-cloud/sandman/internal/metrics"
	"github.com/hauke-cloud/sandman/internal/wol"
)

// IndexNodeRef is the field index that finds devices by the node they point
// at. It is what turns a Node event into the one device that cares about it.
const IndexNodeRef = "spec.nodeRef.name"

// Requeue delays. A transition in flight is checked often, because the whole
// point is to notice the moment a machine appears or disappears; a settled
// device is checked rarely, because nothing is expected to change without an
// event arriving anyway.
const (
	inFlightRequeue = 10 * time.Second
	drainRequeue    = 5 * time.Second
	settledRequeue  = 2 * time.Minute
	// configErrorRequeue paces retries of a device whose spec cannot be used.
	// Nothing sandman does will fix it, so this only has to be often enough to
	// pick up a corrected Secret.
	configErrorRequeue = time.Minute
)

// Reasons recorded in status and on events.
const (
	ReasonNodeReady        = "NodeReady"
	ReasonNodeGone         = "NodeGone"
	ReasonWakeSent         = "WakeSent"
	ReasonWakeFailed       = "WakeFailed"
	ReasonStartTimeout     = "StartTimeout"
	ReasonStopTimeout      = "StopTimeout"
	ReasonDraining         = "Draining"
	ReasonDrainBlocked     = "DrainBlocked"
	ReasonDrainTimeout     = "DrainTimeout"
	ReasonShutdownIssued   = "ShutdownIssued"
	ReasonShutdownFailed   = "ShutdownFailed"
	ReasonShutdownDisabled = "ShutdownDisabled"
	ReasonPaused           = "Paused"
	ReasonUnmanaged        = "Unmanaged"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonObserved         = "Observed"
)

// Waker sends magic packets. It is an interface so the reconciler can be
// tested without a network.
type Waker interface {
	Send(ctx context.Context, target *wol.Target) (int, error)
}

// Reconciler drives every SandmanDevice towards its desired power state.
//
// It is a plain level-based controller: every reconcile reads the world, works
// out the one next step, takes it, and asks to be called again. Nothing is
// held in memory between calls, so a restart mid-transition picks the
// transition up where it was.
type Reconciler struct {
	// Client reads through the informer cache and writes to the API server.
	Client client.Client
	// Reader reads straight from the API server. Used where a cache would be
	// both wasteful and wrong -- listing every pod on a node, reading a
	// Secret.
	Reader client.Reader
	// Scheme resolves owner references.
	Scheme *runtime.Scheme
	// Waker sends the magic packets.
	Waker Waker
	// Namespace is where sandman runs. It is the default home for shutdown
	// pods and for Secrets a device references without naming one.
	Namespace string
	// Metrics records what happened. May be nil.
	Metrics *metrics.Metrics
	// Recorder publishes Kubernetes events. May be nil.
	Recorder recorder.EventRecorder
	// Logger receives the controller's own log.
	Logger *slog.Logger
	// Now returns the current time. Tests replace it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=sandman.hauke.cloud,resources=sandmandevices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sandman.hauke.cloud,resources=sandmandevices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var device v1alpha1.SandmanDevice
	if err := r.Client.Get(ctx, req.NamespacedName, &device); err != nil {
		if apierrors.IsNotFound(err) {
			// The device is gone; take its counters with it so a dashboard
			// does not keep showing a machine nobody manages any more.
			r.Metrics.Forget(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if device.DeletionTimestamp != nil {
		// The shutdown pod is owned by the device, so deletion cleans itself
		// up. There is no finalizer on purpose: blocking the removal of a
		// device on a machine that may be unreachable is a worse failure than
		// leaving a pod behind.
		return ctrl.Result{}, nil
	}

	base := device.DeepCopy()
	result, err := r.reconcile(ctx, &device)
	if err != nil {
		r.Metrics.ObserveReconcileError(device.Name, device.Spec.NodeRef.Name)
	}

	if statusErr := r.patchStatus(ctx, base, &device); statusErr != nil {
		if err == nil {
			return ctrl.Result{}, statusErr
		}
		r.log().Error("writing device status failed", "device", device.Name, "error", statusErr)
	}
	return result, err
}

func (r *Reconciler) reconcile(ctx context.Context, device *v1alpha1.SandmanDevice) (ctrl.Result, error) {
	now := r.now()
	device.Status.ObservedGeneration = device.Generation

	observation, err := observeNode(ctx, r.Client, device.Spec.NodeRef.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("observe node %s: %w", device.Spec.NodeRef.Name, err)
	}
	applyObservation(&device.Status, observation)

	// A device whose spec cannot be turned into a magic packet is reported
	// broken and left alone. Retrying a bad MAC address faster does not make
	// it better, and acting on half a spec is worse than not acting.
	target, err := r.target(ctx, device)
	if err != nil {
		meta.SetStatusCondition(&device.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonInvalidSpec,
			Message:            err.Error(),
			ObservedGeneration: device.Generation,
		})
		device.Status.Reason = ReasonInvalidSpec
		device.Status.Message = err.Error()
		r.event(device, corev1.EventTypeWarning, ReasonInvalidSpec, "Resolving", err.Error())
		return ctrl.Result{RequeueAfter: configErrorRequeue}, nil
	}
	meta.SetStatusCondition(&device.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonObserved,
		Message:            "device configuration resolves",
		ObservedGeneration: device.Generation,
	})

	// A new request id clears a previous failure. Without it a start that
	// timed out once could never be retried without editing the spec, because
	// asking for the same state twice is not a change.
	if requestID := device.Annotations[v1alpha1.AnnotationRequestID]; requestID != device.Status.LastRequestID {
		device.Status.LastRequestID = requestID
		if device.Status.Phase == v1alpha1.PowerPhaseFailed {
			r.setPhase(device, v1alpha1.PowerPhasePending, ReasonObserved, "new power request accepted", now)
		}
	}

	switch device.Spec.Desired() {
	case v1alpha1.PowerStateRunning:
		return r.reconcileOn(ctx, device, observation, target, now)
	case v1alpha1.PowerStateStopped:
		return r.reconcileOff(ctx, device, observation, now)
	default:
		return r.reconcileUnmanaged(device, observation, now)
	}
}

// reconcileUnmanaged reports where the machine is without touching it.
func (r *Reconciler) reconcileUnmanaged(
	device *v1alpha1.SandmanDevice,
	observation nodeObservation,
	now time.Time,
) (ctrl.Result, error) {
	switch {
	case observation.up():
		r.setPhase(device, v1alpha1.PowerPhaseRunning, ReasonUnmanaged, "node is Ready; sandman is not managing this device", now)
	case observation.down(now, device.Spec.Timeouts.StoppedGrace()):
		r.setPhase(device, v1alpha1.PowerPhaseStopped, ReasonUnmanaged, "node is absent or unreachable; sandman is not managing this device", now)
	default:
		r.setPhase(device, v1alpha1.PowerPhaseUnknown, ReasonUnmanaged, "node is registered but not Ready", now)
	}
	r.setPowered(device, observation)
	return ctrl.Result{RequeueAfter: settledRequeue}, nil
}

// reconcileOn drives a machine up.
func (r *Reconciler) reconcileOn(
	ctx context.Context,
	device *v1alpha1.SandmanDevice,
	observation nodeObservation,
	target *wol.Target,
	now time.Time,
) (ctrl.Result, error) {
	// Arriving is checked before anything else, so a machine that came back on
	// its own -- or came back after a start was declared failed -- settles
	// rather than being woken again.
	if observation.up() {
		return r.settleStarted(ctx, device, observation, now)
	}
	if device.Spec.Paused {
		r.setPhase(device, v1alpha1.PowerPhasePending, ReasonPaused, "device is paused; not sending magic packets", now)
		return ctrl.Result{RequeueAfter: settledRequeue}, nil
	}
	if device.Status.Phase == v1alpha1.PowerPhaseFailed {
		// Failed and still asked to be On: wait for a new request rather than
		// retrying forever. A machine that does not answer magic packets needs
		// a person, and a retry loop only hides that.
		return ctrl.Result{RequeueAfter: settledRequeue}, nil
	}

	if device.Status.Phase != v1alpha1.PowerPhaseStarting {
		r.setPhase(device, v1alpha1.PowerPhaseStarting, ReasonWakeSent, "waking machine", now)
		device.Status.WakeAttempts = 0
		device.Status.LastWakeTime = nil
	}

	// The deadline runs from the first packet, so it covers the whole start
	// rather than restarting with every retry.
	if since := device.Status.PhaseSince; since != nil {
		if elapsed := now.Sub(since.Time); elapsed > device.Spec.Timeouts.StartTimeout() {
			message := fmt.Sprintf("machine did not become Ready within %s of the first magic packet (%d sent)",
				device.Spec.Timeouts.StartTimeout(), device.Status.WakeAttempts)
			r.fail(device, ReasonStartTimeout, message, metrics.TransitionStart, metrics.ResultTimeout, now)
			return ctrl.Result{RequeueAfter: settledRequeue}, nil
		}
	}

	if r.wakeDue(device, now) {
		sent, err := r.Waker.Send(ctx, target)
		r.Metrics.ObserveWake(device.Name, device.Spec.NodeRef.Name, sent, err)
		device.Status.LastWakeTime = ptrTime(now)
		device.Status.WakeAttempts += int32(sent) //nolint:gosec // packet counts are single digits
		if err != nil {
			// The packets could not leave this host, which is sandman's
			// problem rather than the machine's -- surface it and retry.
			message := fmt.Sprintf("sending magic packet failed: %v", err)
			device.Status.Reason = ReasonWakeFailed
			device.Status.Message = message
			r.event(device, corev1.EventTypeWarning, ReasonWakeFailed, "Waking", message)
			return ctrl.Result{RequeueAfter: inFlightRequeue}, nil
		}
		r.event(device, corev1.EventTypeNormal, ReasonWakeSent,
			"Waking",
			fmt.Sprintf("sent %d magic packet(s) to %s via %s", sent, target.MAC, target.Broadcast))
		device.Status.Reason = ReasonWakeSent
		device.Status.Message = fmt.Sprintf("magic packets sent, waiting for %s to become Ready",
			device.Spec.NodeRef.Name)
	}

	r.setPowered(device, observation)
	return ctrl.Result{RequeueAfter: nextCheck(device.Spec.Timeouts.WakeRetry())}, nil
}

// settleStarted records that a machine is up and puts the node back to work.
func (r *Reconciler) settleStarted(
	ctx context.Context,
	device *v1alpha1.SandmanDevice,
	observation nodeObservation,
	now time.Time,
) (ctrl.Result, error) {
	if device.Status.Phase == v1alpha1.PowerPhaseStarting && device.Status.PhaseSince != nil {
		r.Metrics.ObserveTransition(device.Name, device.Spec.NodeRef.Name,
			metrics.TransitionStart, metrics.ResultSuccess, now.Sub(device.Status.PhaseSince.Time))
		r.event(device, corev1.EventTypeNormal, ReasonNodeReady,
			"Starting",
			fmt.Sprintf("machine started in %s", now.Sub(device.Status.PhaseSince.Time).Round(time.Second)))
	}

	// The stop that got us here cordoned the node. Leaving it cordoned would
	// mean a machine that is on, costing power, and running nothing.
	if device.Spec.Drain.UncordonEnabled() {
		if changed, err := setSchedulable(ctx, r.Client, observation.node, true); err != nil {
			return ctrl.Result{}, fmt.Errorf("uncordon node %s: %w", device.Spec.NodeRef.Name, err)
		} else if changed {
			r.event(device, corev1.EventTypeNormal, ReasonNodeReady, "Uncordoning", "node uncordoned")
			device.Status.Unschedulable = ptrBool(false)
		}
	}

	// Any shutdown pod still lying about belongs to a previous stop.
	if err := deleteShutdownPods(ctx, r.Client, device, device.Spec.Shutdown.PodNamespace(r.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	if device.Status.Phase != v1alpha1.PowerPhaseRunning {
		device.Status.LastStartedTime = ptrTime(now)
	}
	device.Status.WakeAttempts = 0
	device.Status.ShutdownPod = ""
	r.setPhase(device, v1alpha1.PowerPhaseRunning, ReasonNodeReady, "node is Ready", now)
	r.setPowered(device, observation)
	return ctrl.Result{RequeueAfter: settledRequeue}, nil
}

// reconcileOff drives a machine down: cordon, drain, power off, confirm.
func (r *Reconciler) reconcileOff(
	ctx context.Context,
	device *v1alpha1.SandmanDevice,
	observation nodeObservation,
	now time.Time,
) (ctrl.Result, error) {
	namespace := device.Spec.Shutdown.PodNamespace(r.Namespace)

	if observation.down(now, device.Spec.Timeouts.StoppedGrace()) {
		return r.settleStopped(ctx, device, namespace, now)
	}
	if device.Spec.Paused {
		r.setPhase(device, v1alpha1.PowerPhasePending, ReasonPaused, "device is paused; not stopping the machine", now)
		return ctrl.Result{RequeueAfter: settledRequeue}, nil
	}
	if device.Spec.Shutdown.ShutdownMode() == v1alpha1.ShutdownModeDisabled {
		r.fail(device, ReasonShutdownDisabled,
			"spec.shutdown.mode is Disabled, so sandman will not stop this machine",
			metrics.TransitionStop, metrics.ResultFailure, now)
		return ctrl.Result{RequeueAfter: settledRequeue}, nil
	}
	if device.Status.Phase == v1alpha1.PowerPhaseFailed {
		return ctrl.Result{RequeueAfter: settledRequeue}, nil
	}

	if device.Status.Phase != v1alpha1.PowerPhaseStopping {
		r.setPhase(device, v1alpha1.PowerPhaseStopping, ReasonDraining, "stopping machine", now)
	}
	if since := device.Status.PhaseSince; since != nil {
		if now.Sub(since.Time) > device.Spec.Timeouts.StopTimeout() {
			r.fail(device, ReasonStopTimeout,
				fmt.Sprintf("machine did not stop within %s", device.Spec.Timeouts.StopTimeout()),
				metrics.TransitionStop, metrics.ResultTimeout, now)
			return ctrl.Result{RequeueAfter: settledRequeue}, nil
		}
	}

	// Step one: stop the scheduler sending anything else to a machine that is
	// about to lose power. This happens even when draining is turned off.
	if device.Spec.Drain.CordonEnabled() && observation.node != nil {
		if changed, err := setSchedulable(ctx, r.Client, observation.node, false); err != nil {
			return ctrl.Result{}, fmt.Errorf("cordon node %s: %w", device.Spec.NodeRef.Name, err)
		} else if changed {
			r.event(device, corev1.EventTypeNormal, ReasonDraining, "Cordoning", "node cordoned")
			device.Status.Unschedulable = ptrBool(true)
		}
	}

	// Step two: move the workloads off.
	if device.Spec.Drain.DrainEnabled() {
		done, result, err := r.drain(ctx, device, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			if result.deadlineExceeded {
				return ctrl.Result{RequeueAfter: settledRequeue}, nil
			}
			return ctrl.Result{RequeueAfter: drainRequeue}, nil
		}
	}

	// Step three: ask the machine to power itself off.
	return r.powerOff(ctx, device, namespace, now)
}

type drainProgress struct {
	deadlineExceeded bool
}

// drain runs one pass of the drain and reports whether the node is clear.
func (r *Reconciler) drain(ctx context.Context, device *v1alpha1.SandmanDevice, now time.Time) (bool, drainProgress, error) {
	result, err := drainNode(ctx, r.Client, r.Reader, device, drainOptions{
		ignoreDaemonSets:   device.Spec.Drain.IgnoreDaemonSetPods(),
		deleteEmptyDirData: device.Spec.Drain.DeleteEmptyDir(),
		force:              device.Spec.Drain.ForceDelete(),
		gracePeriodSeconds: device.Spec.Drain.GracePeriod(),
	}, func(err error) {
		r.Metrics.ObserveEviction(device.Name, device.Spec.NodeRef.Name, err)
	})
	if err != nil {
		return false, drainProgress{}, err
	}

	if result.done() {
		if device.Status.PhaseSince != nil && device.Status.LastShutdownTime == nil {
			r.Metrics.ObserveDrain(device.Name, device.Spec.NodeRef.Name, now.Sub(device.Status.PhaseSince.Time))
		}
		return true, drainProgress{}, nil
	}

	device.Status.Reason = ReasonDraining
	device.Status.Message = result.summary()

	// A drain that cannot finish is not forced. Whatever is refusing to move
	// -- a disruption budget, a pod with nowhere to go -- is refusing for a
	// reason, and powering the machine off anyway would override it in the
	// most destructive way available.
	deadline := device.Spec.Drain.DrainTimeout()
	if since := device.Status.PhaseSince; since != nil && now.Sub(since.Time) > deadline {
		reason := ReasonDrainTimeout
		if len(result.blocked) > 0 {
			reason = ReasonDrainBlocked
		}
		r.fail(device, reason,
			fmt.Sprintf("drain did not finish within %s: %s", deadline, result.summary()),
			metrics.TransitionStop, metrics.ResultTimeout, now)
		return false, drainProgress{deadlineExceeded: true}, nil
	}
	return false, drainProgress{}, nil
}

// powerOff creates or watches the shutdown pod.
func (r *Reconciler) powerOff(
	ctx context.Context,
	device *v1alpha1.SandmanDevice,
	namespace string,
	now time.Time,
) (ctrl.Result, error) {
	pods, err := findShutdownPods(ctx, r.Reader, device, namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(pods) == 0 {
		name, err := createShutdownPod(ctx, r.Client, r.Scheme, device, namespace)
		r.Metrics.ObserveShutdown(device.Name, device.Spec.NodeRef.Name, err)
		if err != nil {
			return ctrl.Result{}, err
		}
		device.Status.ShutdownPod = name
		device.Status.LastShutdownTime = ptrTime(now)
		device.Status.Reason = ReasonShutdownIssued
		device.Status.Message = fmt.Sprintf("shutdown pod %s/%s is powering the machine off", namespace, name)
		r.event(device, corev1.EventTypeNormal, ReasonShutdownIssued,
			"Stopping",
			fmt.Sprintf("created privileged shutdown pod %s/%s on %s", namespace, name, device.Spec.NodeRef.Name))
		return ctrl.Result{RequeueAfter: inFlightRequeue}, nil
	}

	device.Status.ShutdownPod = pods[0].Name
	for i := range pods {
		if failure := shutdownPodFailure(&pods[i]); failure != "" {
			r.fail(device, ReasonShutdownFailed, failure, metrics.TransitionStop, metrics.ResultFailure, now)
			return ctrl.Result{RequeueAfter: settledRequeue}, nil
		}
	}

	device.Status.Reason = ReasonShutdownIssued
	device.Status.Message = fmt.Sprintf("waiting for %s to go down", device.Spec.NodeRef.Name)
	return ctrl.Result{RequeueAfter: inFlightRequeue}, nil
}

// settleStopped records that a machine is down and cleans up after it.
func (r *Reconciler) settleStopped(
	ctx context.Context,
	device *v1alpha1.SandmanDevice,
	namespace string,
	now time.Time,
) (ctrl.Result, error) {
	if device.Status.Phase == v1alpha1.PowerPhaseStopping && device.Status.PhaseSince != nil {
		r.Metrics.ObserveTransition(device.Name, device.Spec.NodeRef.Name,
			metrics.TransitionStop, metrics.ResultSuccess, now.Sub(device.Status.PhaseSince.Time))
		r.event(device, corev1.EventTypeNormal, ReasonNodeGone,
			"Stopping",
			fmt.Sprintf("machine stopped in %s", now.Sub(device.Status.PhaseSince.Time).Round(time.Second)))
	}

	// The pod's node is gone, so the pod will never report anything again.
	if err := deleteShutdownPods(ctx, r.Client, device, namespace); err != nil {
		return ctrl.Result{}, err
	}

	if device.Status.Phase != v1alpha1.PowerPhaseStopped {
		device.Status.LastStoppedTime = ptrTime(now)
	}
	device.Status.ShutdownPod = ""
	device.Status.WakeAttempts = 0
	r.setPhase(device, v1alpha1.PowerPhaseStopped, ReasonNodeGone, "node is absent or unreachable", now)
	meta.SetStatusCondition(&device.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPowered,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonNodeGone,
		Message:            "machine is off",
		ObservedGeneration: device.Generation,
	})
	return ctrl.Result{RequeueAfter: settledRequeue}, nil
}

// target resolves a device's spec into a magic packet target.
func (r *Reconciler) target(ctx context.Context, device *v1alpha1.SandmanDevice) (*wol.Target, error) {
	mac, err := wol.ParseMAC(device.Spec.WakeOnLAN.MACAddress)
	if err != nil {
		return nil, err
	}

	target := &wol.Target{
		MAC:       mac,
		Broadcast: device.Spec.WakeOnLAN.BroadcastAddr(),
		Port:      device.Spec.WakeOnLAN.UDPPort(),
		Count:     device.Spec.WakeOnLAN.PacketCount(),
		Interval:  device.Spec.WakeOnLAN.PacketGap(),
	}

	ref := device.Spec.WakeOnLAN.SecureOnSecretRef
	if ref == nil {
		return target, nil
	}

	var secret corev1.Secret
	name := types.NamespacedName{Name: ref.Name, Namespace: ref.SecretNamespace(r.Namespace)}
	// Read through the uncached reader: caching every Secret in the cluster to
	// read one six-byte password would be an unreasonable amount of memory and
	// an unreasonable amount of trust.
	if err := r.Reader.Get(ctx, name, &secret); err != nil {
		return nil, fmt.Errorf("read SecureOn secret %s: %w", name, err)
	}
	raw, ok := secret.Data[ref.SecretKey()]
	if !ok {
		return nil, fmt.Errorf("secret %s has no key %q", name, ref.SecretKey())
	}
	password, err := wol.ParsePassword(raw)
	if err != nil {
		return nil, fmt.Errorf("secret %s key %q: %w", name, ref.SecretKey(), err)
	}
	target.Password = password
	return target, nil
}

// wakeDue reports whether it is time to send another magic packet.
func (r *Reconciler) wakeDue(device *v1alpha1.SandmanDevice, now time.Time) bool {
	last := device.Status.LastWakeTime
	if last == nil {
		return true
	}
	return now.Sub(last.Time) >= device.Spec.Timeouts.WakeRetry()
}

// setPhase moves the device to a phase, stamping the time only on an actual
// change so that a phase's age means what it says.
func (r *Reconciler) setPhase(device *v1alpha1.SandmanDevice, phase v1alpha1.PowerPhase, reason, message string, now time.Time) {
	if device.Status.Phase != phase || device.Status.PhaseSince == nil {
		device.Status.Phase = phase
		device.Status.PhaseSince = ptrTime(now)
	}
	device.Status.Reason = reason
	device.Status.Message = message
}

// fail records a transition that gave up.
func (r *Reconciler) fail(device *v1alpha1.SandmanDevice, reason, message, transition, outcome string, now time.Time) {
	if device.Status.Phase != v1alpha1.PowerPhaseFailed {
		r.Metrics.ObserveTransition(device.Name, device.Spec.NodeRef.Name, transition, outcome, 0)
		r.event(device, corev1.EventTypeWarning, reason, "GivingUp", message)
	}
	r.setPhase(device, v1alpha1.PowerPhaseFailed, reason, message, now)
	meta.SetStatusCondition(&device.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPowered,
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: device.Generation,
	})
}

// setPowered keeps the Powered condition in step with the observation.
func (r *Reconciler) setPowered(device *v1alpha1.SandmanDevice, observation nodeObservation) {
	condition := metav1.Condition{
		Type:               v1alpha1.ConditionPowered,
		Status:             metav1.ConditionUnknown,
		Reason:             string(device.Status.Phase),
		Message:            device.Status.Message,
		ObservedGeneration: device.Generation,
	}
	switch {
	case observation.up():
		condition.Status = metav1.ConditionTrue
		condition.Reason = ReasonNodeReady
	case device.Status.Phase == v1alpha1.PowerPhaseStopped:
		condition.Status = metav1.ConditionFalse
		condition.Reason = ReasonNodeGone
	}
	if condition.Reason == "" {
		condition.Reason = ReasonObserved
	}
	meta.SetStatusCondition(&device.Status.Conditions, condition)
}

// applyObservation copies what was seen of the node into status.
func applyObservation(status *v1alpha1.SandmanDeviceStatus, observation nodeObservation) {
	status.NodeFound = observation.found
	if !observation.found {
		status.NodeReady = nil
		status.Unschedulable = nil
		return
	}
	status.NodeReady = ptrBool(observation.ready)
	status.Unschedulable = ptrBool(observation.unschedulable)
	if observation.address != "" {
		// Kept even once the machine is off: it is the last known place on the
		// network to aim a directed broadcast at.
		status.Address = observation.address
	}
}

// patchStatus writes the status back, and only when something changed.
func (r *Reconciler) patchStatus(ctx context.Context, base, device *v1alpha1.SandmanDevice) error {
	if equalStatus(&base.Status, &device.Status) {
		return nil
	}
	if err := r.Client.Status().Patch(ctx, device, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Deleted or written by someone else mid-reconcile. The next
			// reconcile reads the world again, so there is nothing to fix.
			return nil
		}
		return fmt.Errorf("patch status of %s: %w", device.Name, err)
	}
	return nil
}

// equalStatus reports whether a reconcile changed anything worth writing.
// Semantic equality is used rather than reflect's, so a nil slice and an empty
// one do not count as a change and cause a needless write on every pass.
func equalStatus(a, b *v1alpha1.SandmanDeviceStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// event publishes a Kubernetes event against the device. action names what
// sandman did, which the events API keeps separate from why -- "Waking" as
// against "WakeSent" -- so that `kubectl get events` reads as a sequence of
// actions rather than a list of states.
func (r *Reconciler) event(device *v1alpha1.SandmanDevice, eventType, reason, action, note string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(device, nil, eventType, reason, action, "%s", note)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// nextCheck keeps a transition's polling brisk without letting a long wake
// retry interval delay the check that the machine has arrived.
func nextCheck(retry time.Duration) time.Duration {
	return min(retry, inFlightRequeue)
}

func ptrTime(t time.Time) *metav1.Time {
	out := metav1.NewTime(t)
	return &out
}

func ptrBool(b bool) *bool { return &b }

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Devices are indexed by the node they name so that a Node event can be
	// turned into the device that cares about it without listing everything.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.SandmanDevice{}, IndexNodeRef,
		func(object client.Object) []string {
			device, ok := object.(*v1alpha1.SandmanDevice)
			if !ok || device.Spec.NodeRef.Name == "" {
				return nil
			}
			return []string{device.Spec.NodeRef.Name}
		}); err != nil {
		return fmt.Errorf("index devices by node: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SandmanDevice{}).
		// A node appearing or going Ready is the event a start is waiting for,
		// and a node vanishing is the event a stop is waiting for. Watching
		// them is what makes a transition settle in seconds rather than at the
		// next poll.
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.devicesForNode)).
		Owns(&corev1.Pod{}, builder.MatchEveryOwner).
		Named("sandmandevice").
		Complete(r)
}

func (r *Reconciler) devicesForNode(ctx context.Context, object client.Object) []reconcile.Request {
	var devices v1alpha1.SandmanDeviceList
	if err := r.Client.List(ctx, &devices, client.MatchingFields{IndexNodeRef: object.GetName()}); err != nil {
		r.log().Error("listing devices for node failed", "node", object.GetName(), "error", err)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(devices.Items))
	for i := range devices.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: devices.Items[i].Name},
		})
	}
	return requests
}
