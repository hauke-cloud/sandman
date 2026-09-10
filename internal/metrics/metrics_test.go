package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
)

var now = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func testDevice(mutators ...func(*v1alpha1.SandmanDevice)) v1alpha1.SandmanDevice {
	device := v1alpha1.SandmanDevice{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-01"},
		Spec: v1alpha1.SandmanDeviceSpec{
			NodeRef:           v1alpha1.NodeReference{Name: "worker-01"},
			DesiredPowerState: v1alpha1.PowerStateRunning,
			WakeOnLAN: v1alpha1.WakeOnLANSpec{
				MACAddress:       "aa:bb:cc:dd:ee:ff",
				BroadcastAddress: "192.168.1.255",
			},
		},
		Status: v1alpha1.SandmanDeviceStatus{
			Phase:      v1alpha1.PowerPhaseRunning,
			NodeFound:  true,
			PhaseSince: &metav1.Time{Time: now},
		},
	}
	for _, mutate := range mutators {
		mutate(&device)
	}
	return device
}

func collector(devices ...v1alpha1.SandmanDevice) *DeviceCollector {
	return NewDeviceCollector(func(_ context.Context) ([]v1alpha1.SandmanDevice, error) {
		return devices, nil
	}, nil)
}

func TestDeviceCollectorExportsOneSeriesPerState(t *testing.T) {
	c := collector(testDevice())

	got := testutil.CollectAndCount(c, "sandman_device_power_state")

	if want := len(allPhases); got != want {
		t.Fatalf("power_state series = %d, want %d -- one per state so a panel does not go blank on a change", got, want)
	}
}

func TestDeviceCollectorMarksTheCurrentState(t *testing.T) {
	c := collector(testDevice())

	expected := `
# HELP sandman_device_power_state Observed power state, 1 on the state that applies. States: Unknown, Pending, Starting, Running, Stopping, Stopped, Failed.
# TYPE sandman_device_power_state gauge
sandman_device_power_state{device="worker-01",node="worker-01",state="Failed"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Pending"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Running"} 1
sandman_device_power_state{device="worker-01",node="worker-01",state="Starting"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Stopped"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Stopping"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "sandman_device_power_state"); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceCollectorReportsAnEmptyPhaseAsUnknown(t *testing.T) {
	c := collector(testDevice(func(d *v1alpha1.SandmanDevice) { d.Status.Phase = "" }))

	expected := `
# HELP sandman_device_power_state Observed power state, 1 on the state that applies. States: Unknown, Pending, Starting, Running, Stopping, Stopped, Failed.
# TYPE sandman_device_power_state gauge
sandman_device_power_state{device="worker-01",node="worker-01",state="Failed"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Pending"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Running"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Starting"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Stopped"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Stopping"} 0
sandman_device_power_state{device="worker-01",node="worker-01",state="Unknown"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "sandman_device_power_state"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeReadyIsAbsentWithoutANode(t *testing.T) {
	c := collector(testDevice(func(d *v1alpha1.SandmanDevice) {
		d.Status.NodeFound = false
		d.Status.NodeReady = nil
	}))

	if got := testutil.CollectAndCount(c, "sandman_device_node_ready"); got != 0 {
		t.Fatalf("node_ready series = %d, want 0: there is no node to report readiness of", got)
	}
	if got := testutil.CollectAndCount(c, "sandman_device_node_found"); got != 1 {
		t.Fatalf("node_found series = %d, want 1", got)
	}
}

// The deadline is what an alert compares against, so exporting it while nothing
// is in flight would make every settled device look overdue.
func TestTransitionDeadlineIsOnlyExportedWhileInFlight(t *testing.T) {
	settled := collector(testDevice())
	if got := testutil.CollectAndCount(settled, "sandman_device_transition_deadline_timestamp_seconds"); got != 0 {
		t.Fatalf("deadline series = %d for a settled device, want 0", got)
	}

	starting := collector(testDevice(func(d *v1alpha1.SandmanDevice) {
		d.Status.Phase = v1alpha1.PowerPhaseStarting
	}))
	if got := testutil.CollectAndCount(starting, "sandman_device_transition_deadline_timestamp_seconds"); got != 1 {
		t.Fatalf("deadline series = %d for a starting device, want 1", got)
	}
}

func TestTransitionDeadlineUsesTheRelevantTimeout(t *testing.T) {
	stopping := collector(testDevice(func(d *v1alpha1.SandmanDevice) {
		d.Status.Phase = v1alpha1.PowerPhaseStopping
		d.Spec.Timeouts.Stop = &metav1.Duration{Duration: 3 * time.Minute}
		d.Spec.Timeouts.Start = &metav1.Duration{Duration: time.Hour}
	}))

	value := testutil.ToFloat64(deadlineOnly(t, stopping))

	if want := float64(now.Add(3 * time.Minute).Unix()); value != want {
		t.Fatalf("deadline = %v, want %v -- a stop must be measured against the stop timeout", value, want)
	}
}

// deadlineOnly wraps a collector so ToFloat64 sees a single series.
func deadlineOnly(t *testing.T, c prometheus.Collector) prometheus.Collector {
	t.Helper()
	return filteredCollector{inner: c, keep: "sandman_device_transition_deadline_timestamp_seconds"}
}

type filteredCollector struct {
	inner prometheus.Collector
	keep  string
}

func (f filteredCollector) Describe(chan<- *prometheus.Desc) {}

func (f filteredCollector) Collect(ch chan<- prometheus.Metric) {
	inner := make(chan prometheus.Metric, 256)
	go func() {
		f.inner.Collect(inner)
		close(inner)
	}()
	for metric := range inner {
		if strings.Contains(metric.Desc().String(), f.keep) {
			ch <- metric
		}
	}
}

func TestCollectFailureIsReported(t *testing.T) {
	c := NewDeviceCollector(func(_ context.Context) ([]v1alpha1.SandmanDevice, error) {
		return nil, errors.New("cache is not synced")
	}, nil)

	expected := `
# HELP sandman_device_collect_failures_total 1 when the last scrape could not read the device list.
# TYPE sandman_device_collect_failures_total gauge
sandman_device_collect_failures_total 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "sandman_device_collect_failures_total"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectAndCount(c, "sandman_device_power_state"); got != 0 {
		t.Fatalf("a failed collect must not invent device series, got %d", got)
	}
}

func TestObserveWakeCountsPacketsAndAttempts(t *testing.T) {
	m := New(prometheus.NewRegistry())

	m.ObserveWake("worker-01", "worker-01", 3, nil)
	m.ObserveWake("worker-01", "worker-01", 0, errors.New("network unreachable"))

	if got := testutil.ToFloat64(m.WakePacketsTotal.WithLabelValues("worker-01", "worker-01")); got != 3 {
		t.Fatalf("packets = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.WakeAttemptsTotal.WithLabelValues("worker-01", "worker-01", ResultSuccess)); got != 1 {
		t.Fatalf("successful attempts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.WakeAttemptsTotal.WithLabelValues("worker-01", "worker-01", ResultFailure)); got != 1 {
		t.Fatalf("failed attempts = %v, want 1", got)
	}
}

func TestTransitionDurationIsOnlyObservedOnSuccess(t *testing.T) {
	m := New(prometheus.NewRegistry())

	m.ObserveTransition("worker-01", "worker-01", TransitionStart, ResultTimeout, 5*time.Minute)
	if got := testutil.CollectAndCount(m.TransitionDurationSeconds); got != 0 {
		t.Fatal("a transition that timed out has no duration worth putting in a histogram")
	}

	m.ObserveTransition("worker-01", "worker-01", TransitionStart, ResultSuccess, 42*time.Second)
	if got := testutil.CollectAndCount(m.TransitionDurationSeconds); got != 1 {
		t.Fatalf("histogram series = %d, want 1", got)
	}
}

func TestForgetDropsEverySeriesOfADeletedDevice(t *testing.T) {
	m := New(prometheus.NewRegistry())
	m.ObserveWake("worker-01", "worker-01", 3, nil)
	m.ObserveWake("worker-02", "worker-02", 3, nil)
	m.ObserveShutdown("worker-01", "worker-01", nil)

	m.Forget("worker-01")

	if got := testutil.CollectAndCount(m.WakePacketsTotal); got != 1 {
		t.Fatalf("remaining packet series = %d, want only worker-02's", got)
	}
	if got := testutil.CollectAndCount(m.ShutdownAttemptsTotal); got != 0 {
		t.Fatalf("remaining shutdown series = %d, want 0", got)
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics

	// The reconciler is constructed with metrics in production and without
	// them in some tests; neither should have to nil-check at every call site.
	m.ObserveWake("a", "b", 1, nil)
	m.ObserveShutdown("a", "b", nil)
	m.ObserveEviction("a", "b", nil)
	m.ObserveDrain("a", "b", time.Second)
	m.ObserveTransition("a", "b", TransitionStop, ResultSuccess, time.Second)
	m.ObserveReconcileError("a", "b")
	m.Forget("a")
}

func TestRegistryCarriesBuildInfo(t *testing.T) {
	reg := NewRegistry()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, family := range families {
		if family.GetName() == "sandman_build_info" {
			found = true
		}
	}
	if !found {
		t.Fatal("the registry must carry build info, so a dashboard can say which version is running")
	}
}
