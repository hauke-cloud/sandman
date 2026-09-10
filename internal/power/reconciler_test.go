package power

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
	"github.com/hauke-cloud/sandman/internal/metrics"
	"github.com/hauke-cloud/sandman/internal/wol"
)

const (
	deviceName = "worker-01"
	nodeName   = "worker-01"
	namespace  = "sandman"
)

var testNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// fakeWaker records the magic packets a reconcile would have sent.
type fakeWaker struct {
	targets []*wol.Target
	sent    int
	err     error
}

func (w *fakeWaker) Send(_ context.Context, target *wol.Target) (int, error) {
	w.targets = append(w.targets, target)
	if w.err != nil {
		return 0, w.err
	}
	if w.sent == 0 {
		return len(target.MAC) / 2, nil // deterministic, non-zero
	}
	return w.sent, nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("sandman scheme: %v", err)
	}
	return scheme
}

type harness struct {
	reconciler *Reconciler
	client     client.Client
	waker      *fakeWaker
	events     *events.FakeRecorder
	now        time.Time
}

func newHarness(t *testing.T, objects ...client.Object) *harness {
	t.Helper()

	scheme := testScheme(t)
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.SandmanDevice{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(object client.Object) []string {
			pod, ok := object.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}).
		WithObjects(objects...)

	c := builder.Build()
	h := &harness{
		client: c,
		waker:  &fakeWaker{},
		events: events.NewFakeRecorder(64),
		now:    testNow,
	}
	h.reconciler = &Reconciler{
		Client:    c,
		Reader:    c,
		Scheme:    scheme,
		Waker:     h.waker,
		Namespace: namespace,
		Metrics:   metrics.New(prometheusRegistry(t)),
		Recorder:  h.events,
		Now:       func() time.Time { return h.now },
	}
	return h
}

func (h *harness) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	result, err := h.reconciler.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: deviceName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return result
}

func (h *harness) device(t *testing.T) *v1alpha1.SandmanDevice {
	t.Helper()
	var device v1alpha1.SandmanDevice
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: deviceName}, &device); err != nil {
		t.Fatalf("get device: %v", err)
	}
	return &device
}

func (h *harness) pods(t *testing.T) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := h.client.List(context.Background(), &pods, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return pods.Items
}

func newDevice(mutators ...func(*v1alpha1.SandmanDevice)) *v1alpha1.SandmanDevice {
	device := &v1alpha1.SandmanDevice{
		ObjectMeta: metav1.ObjectMeta{Name: deviceName, Generation: 1},
		Spec: v1alpha1.SandmanDeviceSpec{
			NodeRef: v1alpha1.NodeReference{Name: nodeName},
			WakeOnLAN: v1alpha1.WakeOnLANSpec{
				MACAddress:       "aa:bb:cc:dd:ee:ff",
				BroadcastAddress: "192.168.1.255",
			},
		},
	}
	for _, mutate := range mutators {
		mutate(device)
	}
	return device
}

func desired(state v1alpha1.PowerState) func(*v1alpha1.SandmanDevice) {
	return func(d *v1alpha1.SandmanDevice) { d.Spec.DesiredPowerState = state }
}

func newNode(ready bool, mutators ...func(*corev1.Node)) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:              corev1.NodeReady,
				Status:            status,
				LastHeartbeatTime: metav1.NewTime(testNow),
			}},
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.20"}},
		},
	}
	for _, mutate := range mutators {
		mutate(node)
	}
	return node
}

func TestStartSendsMagicPacketAndWaits(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)))

	result := h.reconcile(t)

	if len(h.waker.targets) != 1 {
		t.Fatalf("sent %d wake targets, want 1", len(h.waker.targets))
	}
	if got := h.waker.targets[0].Broadcast; got != "192.168.1.255" {
		t.Fatalf("broadcast = %q, want the device's own", got)
	}
	if got := h.waker.targets[0].Port; got != v1alpha1.DefaultPort {
		t.Fatalf("port = %d, want the default %d", got, v1alpha1.DefaultPort)
	}

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseStarting {
		t.Fatalf("phase = %q, want Starting", device.Status.Phase)
	}
	if device.Status.LastWakeTime == nil {
		t.Fatal("a sent packet must be recorded in status")
	}
	if device.Status.WakeAttempts == 0 {
		t.Fatal("wake attempts must count the packets sent")
	}
	if result.RequeueAfter == 0 {
		t.Fatal("a start in flight must ask to be looked at again")
	}
}

func TestStartDoesNotResendBeforeTheRetryInterval(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)))

	h.reconcile(t)
	// Well inside the one-minute default retry interval.
	h.now = testNow.Add(5 * time.Second)
	h.reconcile(t)

	if len(h.waker.targets) != 1 {
		t.Fatalf("sent %d times, want 1: a magic packet every reconcile would be a packet every few seconds", len(h.waker.targets))
	}
}

func TestStartResendsAfterTheRetryInterval(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)))

	h.reconcile(t)
	h.now = testNow.Add(v1alpha1.DefaultWakeRetryInterval + time.Second)
	h.reconcile(t)

	if len(h.waker.targets) != 2 {
		t.Fatalf("sent %d times, want 2: a sleeping NIC may have missed the first", len(h.waker.targets))
	}
}

func TestStartFailsAfterTheDeadline(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)))

	h.reconcile(t)
	h.now = testNow.Add(v1alpha1.DefaultStartTimeout + time.Minute)
	h.reconcile(t)

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseFailed {
		t.Fatalf("phase = %q, want Failed", device.Status.Phase)
	}
	if device.Status.Reason != ReasonStartTimeout {
		t.Fatalf("reason = %q, want %q", device.Status.Reason, ReasonStartTimeout)
	}
}

func TestFailedStartIsNotRetriedOnItsOwn(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)))

	h.reconcile(t)
	h.now = testNow.Add(v1alpha1.DefaultStartTimeout + time.Minute)
	h.reconcile(t)
	before := len(h.waker.targets)

	h.now = h.now.Add(time.Hour)
	h.reconcile(t)

	if len(h.waker.targets) != before {
		t.Fatal("a failed start must wait for a new request rather than retrying forever")
	}
}

func TestNewRequestRetriesAFailedStart(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)))

	h.reconcile(t)
	h.now = testNow.Add(v1alpha1.DefaultStartTimeout + time.Minute)
	h.reconcile(t)

	device := h.device(t)
	device.Annotations = map[string]string{v1alpha1.AnnotationRequestID: "request-2"}
	if err := h.client.Update(context.Background(), device); err != nil {
		t.Fatalf("update: %v", err)
	}

	before := len(h.waker.targets)
	h.reconcile(t)

	if len(h.waker.targets) <= before {
		t.Fatal("a new request id must clear the failure and wake the machine again")
	}
	if got := h.device(t).Status.LastRequestID; got != "request-2" {
		t.Fatalf("status.lastRequestID = %q, want the id acted on", got)
	}
}

func TestReadyNodeSettlesStarted(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)), newNode(true, func(n *corev1.Node) {
		n.Spec.Unschedulable = true
	}))

	h.reconcile(t)

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseRunning {
		t.Fatalf("phase = %q, want Started", device.Status.Phase)
	}
	if device.Status.LastStartedTime == nil {
		t.Fatal("reaching Started must be timestamped")
	}
	if device.Status.Address != "192.168.1.20" {
		t.Fatalf("address = %q, want the node's internal IP", device.Status.Address)
	}
	if len(h.waker.targets) != 0 {
		t.Fatal("a machine that is already up must not be woken")
	}

	var node corev1.Node
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.Spec.Unschedulable {
		t.Fatal("a started machine must be uncordoned, or it comes back on and runs nothing")
	}
}

func TestStopCordonsAndCreatesTheShutdownPod(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateStopped)), newNode(true))

	h.reconcile(t)

	var node corev1.Node
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !node.Spec.Unschedulable {
		t.Fatal("a machine about to lose power must be cordoned first")
	}

	pods := h.pods(t)
	if len(pods) != 1 {
		t.Fatalf("created %d pods, want 1", len(pods))
	}
	pod := pods[0]
	if pod.Spec.NodeName != nodeName {
		t.Fatalf("pod placed on %q, want %q", pod.Spec.NodeName, nodeName)
	}
	if !pod.Spec.HostPID {
		t.Fatal("hostPID is what lets nsenter reach the host's systemd")
	}
	if pod.Spec.Containers[0].SecurityContext == nil || !*pod.Spec.Containers[0].SecurityContext.Privileged {
		t.Fatal("the shutdown container has to be privileged")
	}
	if len(pod.Spec.Tolerations) == 0 {
		t.Fatal("the shutdown pod must tolerate the taints of the node it is stopping")
	}

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseStopping {
		t.Fatalf("phase = %q, want Stopping", device.Status.Phase)
	}
	if device.Status.ShutdownPod != pod.Name {
		t.Fatalf("status.shutdownPod = %q, want %q", device.Status.ShutdownPod, pod.Name)
	}
}

func TestStopDoesNotCreateASecondShutdownPod(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateStopped)), newNode(true))

	h.reconcile(t)
	h.now = testNow.Add(30 * time.Second)
	h.reconcile(t)

	if pods := h.pods(t); len(pods) != 1 {
		t.Fatalf("created %d shutdown pods, want 1", len(pods))
	}
}

func TestVanishedNodeSettlesStopped(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateStopped)), newNode(true))

	h.reconcile(t)

	var node corev1.Node
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if err := h.client.Delete(context.Background(), &node); err != nil {
		t.Fatalf("delete node: %v", err)
	}

	h.now = testNow.Add(time.Minute)
	h.reconcile(t)

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseStopped {
		t.Fatalf("phase = %q, want Stopped", device.Status.Phase)
	}
	if device.Status.LastStoppedTime == nil {
		t.Fatal("reaching Stopped must be timestamped")
	}
	if pods := h.pods(t); len(pods) != 0 {
		t.Fatalf("%d shutdown pods left behind; their node is gone and they will never report again", len(pods))
	}
}

// A node whose machine loses power keeps its Ready condition at whatever it
// last said. Only the age of the heartbeat gives it away.
func TestUnreachableNodeCountsAsStoppedOnlyAfterTheGracePeriod(t *testing.T) {
	stale := func(age time.Duration) *corev1.Node {
		return newNode(false, func(n *corev1.Node) {
			n.Status.Conditions[0].Status = corev1.ConditionUnknown
			n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(testNow.Add(-age))
		})
	}

	fresh := newHarness(t, newDevice(desired(v1alpha1.PowerStateStopped)), stale(30*time.Second))
	fresh.reconcile(t)
	if got := fresh.device(t).Status.Phase; got == v1alpha1.PowerPhaseStopped {
		t.Fatal("a node unreachable for 30s is not yet evidence the machine is off")
	}

	old := newHarness(t, newDevice(desired(v1alpha1.PowerStateStopped)), stale(v1alpha1.DefaultStoppedGracePeriod+time.Minute))
	old.reconcile(t)
	if got := old.device(t).Status.Phase; got != v1alpha1.PowerPhaseStopped {
		t.Fatalf("phase = %q, want Stopped once the heartbeat is older than the grace period", got)
	}
}

func TestStopFailsWhenShutdownIsDisabled(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateStopped), func(d *v1alpha1.SandmanDevice) {
		d.Spec.Shutdown.Mode = v1alpha1.ShutdownModeDisabled
	}), newNode(true))

	h.reconcile(t)

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseFailed {
		t.Fatalf("phase = %q, want Failed", device.Status.Phase)
	}
	if device.Status.Reason != ReasonShutdownDisabled {
		t.Fatalf("reason = %q, want %q", device.Status.Reason, ReasonShutdownDisabled)
	}
	if pods := h.pods(t); len(pods) != 0 {
		t.Fatal("no shutdown pod may be created when shutdown is disabled")
	}
}

func TestPausedDeviceIsNotActedOn(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning), func(d *v1alpha1.SandmanDevice) {
		d.Spec.Paused = true
	}))

	h.reconcile(t)

	if len(h.waker.targets) != 0 {
		t.Fatal("a paused device must not be woken")
	}
	if got := h.device(t).Status.Phase; got != v1alpha1.PowerPhasePending {
		t.Fatalf("phase = %q, want Pending", got)
	}
	if got := h.device(t).Status.Reason; got != ReasonPaused {
		t.Fatalf("reason = %q, want %q", got, ReasonPaused)
	}
}

func TestUnmanagedDeviceIsOnlyObserved(t *testing.T) {
	h := newHarness(t, newDevice(), newNode(true))

	h.reconcile(t)

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseRunning {
		t.Fatalf("phase = %q, want Started: an unmanaged device is still reported", device.Status.Phase)
	}
	if device.Status.Reason != ReasonUnmanaged {
		t.Fatalf("reason = %q, want %q", device.Status.Reason, ReasonUnmanaged)
	}
	if len(h.pods(t)) != 0 || len(h.waker.targets) != 0 {
		t.Fatal("an unmanaged device must never be acted on")
	}
}

func TestInvalidMACIsReportedWithoutActing(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning), func(d *v1alpha1.SandmanDevice) {
		d.Spec.WakeOnLAN.MACAddress = "not-a-mac"
	}))

	result := h.reconcile(t)

	if len(h.waker.targets) != 0 {
		t.Fatal("a device with an unusable MAC must not be acted on")
	}
	device := h.device(t)
	if device.Status.Reason != ReasonInvalidSpec {
		t.Fatalf("reason = %q, want %q", device.Status.Reason, ReasonInvalidSpec)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("a broken spec should still be re-checked, in case it is corrected")
	}
}

func TestSecureOnPasswordIsReadFromItsSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wol-password", Namespace: namespace},
		Data:       map[string][]byte{"password": []byte("01:02:03:04:05:06")},
	}
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning), func(d *v1alpha1.SandmanDevice) {
		d.Spec.WakeOnLAN.SecureOnSecretRef = &v1alpha1.SecretKeyReference{Name: "wol-password"}
	}), secret)

	h.reconcile(t)

	if len(h.waker.targets) != 1 {
		t.Fatalf("sent %d targets, want 1", len(h.waker.targets))
	}
	want := []byte{1, 2, 3, 4, 5, 6}
	got := h.waker.targets[0].Password
	if len(got) != len(want) {
		t.Fatalf("password = % x, want % x", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("password = % x, want % x", got, want)
		}
	}
}

func TestMissingSecureOnSecretIsAConfigurationError(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning), func(d *v1alpha1.SandmanDevice) {
		d.Spec.WakeOnLAN.SecureOnSecretRef = &v1alpha1.SecretKeyReference{Name: "missing"}
	}))

	h.reconcile(t)

	if len(h.waker.targets) != 0 {
		t.Fatal("a device whose password cannot be read must not be woken with no password")
	}
	if got := h.device(t).Status.Reason; got != ReasonInvalidSpec {
		t.Fatalf("reason = %q, want %q", got, ReasonInvalidSpec)
	}
}

func TestWakeFailureIsReportedAndRetried(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)))
	h.waker.err = errors.New("network is unreachable")

	result := h.reconcile(t)

	device := h.device(t)
	if device.Status.Phase != v1alpha1.PowerPhaseStarting {
		t.Fatalf("phase = %q, want Starting: the machine has not been given up on", device.Status.Phase)
	}
	if device.Status.Reason != ReasonWakeFailed {
		t.Fatalf("reason = %q, want %q", device.Status.Reason, ReasonWakeFailed)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("a failed send must be retried")
	}
}

func TestMissingDeviceIsNotAnError(t *testing.T) {
	h := newHarness(t)

	if _, err := h.reconciler.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone"}}); err != nil {
		t.Fatalf("reconciling a deleted device must be a no-op, got %v", err)
	}
}

func TestPhaseSinceOnlyMovesOnAnActualChange(t *testing.T) {
	h := newHarness(t, newDevice(desired(v1alpha1.PowerStateRunning)), newNode(true))

	h.reconcile(t)
	first := h.device(t).Status.PhaseSince

	h.now = testNow.Add(time.Hour)
	h.reconcile(t)
	second := h.device(t).Status.PhaseSince

	if first == nil || second == nil {
		t.Fatal("phaseSince must be set")
	}
	if !first.Time.Equal(second.Time) {
		t.Fatalf("phaseSince moved from %s to %s without the phase changing", first, second)
	}
}
