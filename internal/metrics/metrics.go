// Package metrics holds sandman's Prometheus surface: counters and histograms
// the controller writes as it acts, and a collector that reads the current
// state of every device at scrape time.
//
// The split matters. Anything cumulative -- packets sent, shutdowns attempted,
// transitions finished -- is a counter owned by the code that does the thing.
// Anything describing how a device *is* right now is read from the informer
// cache when Prometheus asks, so a deleted SandmanDevice takes its series with
// it instead of leaving a last value behind forever.
package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
	"github.com/hauke-cloud/sandman/internal/version"
)

// Namespace prefixes every metric.
const Namespace = "sandman"

// Result values used on the outcome label of the counters below.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultTimeout = "timeout"
)

// Transition values, naming the direction a device was moved in.
const (
	TransitionStart = "start"
	TransitionStop  = "stop"
)

// Metrics is the set of instruments the controller and the API server write.
type Metrics struct {
	// WakePacketsTotal counts magic packets that left the host. A wake with
	// packets=3 adds three, because a NIC that misses two of them is a very
	// different situation from one that misses all three.
	WakePacketsTotal *prometheus.CounterVec
	// WakeAttemptsTotal counts wake attempts, whatever their packet count.
	WakeAttemptsTotal *prometheus.CounterVec
	// ShutdownAttemptsTotal counts shutdown pods created, by outcome.
	ShutdownAttemptsTotal *prometheus.CounterVec
	// EvictionsTotal counts pod evictions issued while draining.
	EvictionsTotal *prometheus.CounterVec
	// DrainDurationSeconds measures a completed drain.
	DrainDurationSeconds *prometheus.HistogramVec
	// TransitionsTotal counts finished transitions, by direction and outcome.
	TransitionsTotal *prometheus.CounterVec
	// TransitionDurationSeconds measures how long a settled transition took --
	// for a start, the time from the first magic packet to a Ready node, which
	// is the number an operator actually wants to know.
	TransitionDurationSeconds *prometheus.HistogramVec
	// ReconcileErrorsTotal counts reconciles that returned an error.
	ReconcileErrorsTotal *prometheus.CounterVec
	// APIRequestsTotal counts REST requests by route, method and status.
	APIRequestsTotal *prometheus.CounterVec
	// APIRequestDurationSeconds measures REST request latency.
	APIRequestDurationSeconds *prometheus.HistogramVec
}

// deviceLabels are carried by every per-device instrument. The device name and
// the node name are usually the same string, and are kept separate anyway
// because nothing stops them differing and a dashboard joining on the wrong
// one is hard to spot.
const (
	labelDevice = "device"
	labelNode   = "node"
)

var deviceLabels = []string{labelDevice, labelNode}

// withLabels returns the per-device labels plus the extras a particular
// instrument needs.
func withLabels(extra ...string) []string {
	labels := make([]string, 0, len(deviceLabels)+len(extra))
	labels = append(labels, deviceLabels...)
	return append(labels, extra...)
}

// New builds the instruments and registers them.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		WakePacketsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "wake_packets_sent_total",
			Help:      "Wake-on-LAN magic packets written to the network, by device.",
		}, deviceLabels),
		WakeAttemptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "wake_attempts_total",
			Help:      "Wake attempts by outcome. A failure means the packets could not be sent, not that the machine stayed down.",
		}, withLabels("result")),
		ShutdownAttemptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "shutdown_attempts_total",
			Help:      "Shutdown pods created, by outcome.",
		}, withLabels("result")),
		EvictionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "pod_evictions_total",
			Help:      "Pod evictions issued while draining a node, by outcome. A blocked eviction is a PodDisruptionBudget doing its job, not an error.",
		}, withLabels("result")),
		DrainDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "drain_duration_seconds",
			Help:      "Time from the start of a drain to the last pod leaving the node.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300, 600},
		}, deviceLabels),
		TransitionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "power_transitions_total",
			Help:      "Completed power transitions, by direction and outcome.",
		}, withLabels("transition", "result")),
		TransitionDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "power_transition_duration_seconds",
			Help:      "Time a power transition took to settle. For a start this is first magic packet to Ready node; hardware, firmware and the kubelet's first heartbeat are all inside it.",
			// Spread for a physical machine: a fast NUC settles in under a
			// minute, a server with a long POST takes several.
			Buckets: []float64{10, 20, 30, 45, 60, 90, 120, 180, 300, 600, 900},
		}, withLabels("transition")),
		ReconcileErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "reconcile_errors_total",
			Help:      "Reconciles that ended in an error.",
		}, deviceLabels),
		APIRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "api_requests_total",
			Help:      "REST API requests by route, method and status code.",
		}, []string{"route", "method", "code"}),
		APIRequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "api_request_duration_seconds",
			Help:      "REST API request latency by route and method.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"route", "method"}),
	}

	reg.MustRegister(
		m.WakePacketsTotal,
		m.WakeAttemptsTotal,
		m.ShutdownAttemptsTotal,
		m.EvictionsTotal,
		m.DrainDurationSeconds,
		m.TransitionsTotal,
		m.TransitionDurationSeconds,
		m.ReconcileErrorsTotal,
		m.APIRequestsTotal,
		m.APIRequestDurationSeconds,
	)
	return m
}

// NewRegistry returns a registry carrying the Go, process and build-info
// collectors, ready for sandman's own instruments.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		version.Collector(),
	)
	return reg
}

// ObserveWake records a wake attempt.
func (m *Metrics) ObserveWake(device, node string, packets int, err error) {
	if m == nil {
		return
	}
	if packets > 0 {
		m.WakePacketsTotal.WithLabelValues(device, node).Add(float64(packets))
	}
	m.WakeAttemptsTotal.WithLabelValues(device, node, result(err)).Inc()
}

// ObserveShutdown records a shutdown pod creation.
func (m *Metrics) ObserveShutdown(device, node string, err error) {
	if m == nil {
		return
	}
	m.ShutdownAttemptsTotal.WithLabelValues(device, node, result(err)).Inc()
}

// ObserveEviction records one eviction call.
func (m *Metrics) ObserveEviction(device, node string, err error) {
	if m == nil {
		return
	}
	m.EvictionsTotal.WithLabelValues(device, node, result(err)).Inc()
}

// ObserveDrain records a finished drain.
func (m *Metrics) ObserveDrain(device, node string, d time.Duration) {
	if m == nil {
		return
	}
	m.DrainDurationSeconds.WithLabelValues(device, node).Observe(d.Seconds())
}

// ObserveTransition records a settled transition.
func (m *Metrics) ObserveTransition(device, node, transition, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.TransitionsTotal.WithLabelValues(device, node, transition, outcome).Inc()
	if outcome == ResultSuccess && d > 0 {
		m.TransitionDurationSeconds.WithLabelValues(device, node, transition).Observe(d.Seconds())
	}
}

// ObserveReconcileError records a failed reconcile.
func (m *Metrics) ObserveReconcileError(device, node string) {
	if m == nil {
		return
	}
	m.ReconcileErrorsTotal.WithLabelValues(device, node).Inc()
}

// Forget drops every per-device series for a device that no longer exists.
// Counters are not read from the cache the way the gauges are, so without this
// a deleted device's totals would sit at their last value until a restart.
func (m *Metrics) Forget(device string) {
	if m == nil {
		return
	}
	labels := prometheus.Labels{labelDevice: device}
	m.WakePacketsTotal.DeletePartialMatch(labels)
	m.WakeAttemptsTotal.DeletePartialMatch(labels)
	m.ShutdownAttemptsTotal.DeletePartialMatch(labels)
	m.EvictionsTotal.DeletePartialMatch(labels)
	m.DrainDurationSeconds.DeletePartialMatch(labels)
	m.TransitionsTotal.DeletePartialMatch(labels)
	m.TransitionDurationSeconds.DeletePartialMatch(labels)
	m.ReconcileErrorsTotal.DeletePartialMatch(labels)
}

func result(err error) string {
	if err != nil {
		return ResultFailure
	}
	return ResultSuccess
}

// Lister returns every device sandman knows about. It is satisfied by a read
// against the controller's informer cache, which is why collecting is cheap
// enough to do on every scrape.
type Lister func(ctx context.Context) ([]v1alpha1.SandmanDevice, error)

// DeviceCollector exports the current state of every device.
type DeviceCollector struct {
	list    Lister
	logger  *slog.Logger
	timeout time.Duration

	info            *prometheus.Desc
	state           *prometheus.Desc
	desired         *prometheus.Desc
	paused          *prometheus.Desc
	nodeFound       *prometheus.Desc
	nodeReady       *prometheus.Desc
	unschedulable   *prometheus.Desc
	phaseSince      *prometheus.Desc
	wakeAttempts    *prometheus.Desc
	lastWake        *prometheus.Desc
	lastShutdown    *prometheus.Desc
	lastStarted     *prometheus.Desc
	lastStopped     *prometheus.Desc
	transitionUntil *prometheus.Desc
	scrapeError     *prometheus.Desc
}

// allPhases is every value the state enum can take. Exporting a series per
// phase with a 0 on the ones that do not apply is what makes
// `sandman_device_power_state == 1` a usable query, and what keeps a panel
// from going blank when a device changes phase.
var allPhases = []v1alpha1.PowerPhase{
	v1alpha1.PowerPhaseUnknown,
	v1alpha1.PowerPhasePending,
	v1alpha1.PowerPhaseStarting,
	v1alpha1.PowerPhaseRunning,
	v1alpha1.PowerPhaseStopping,
	v1alpha1.PowerPhaseStopped,
	v1alpha1.PowerPhaseFailed,
}

var allStates = []v1alpha1.PowerState{
	v1alpha1.PowerStateRunning,
	v1alpha1.PowerStateStopped,
	v1alpha1.PowerStateUnmanaged,
}

// NewDeviceCollector returns a collector reading through list.
func NewDeviceCollector(list Lister, logger *slog.Logger) *DeviceCollector {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeviceCollector{
		list:    list,
		logger:  logger,
		timeout: 5 * time.Second,
		info: prometheus.NewDesc(
			Namespace+"_device_info",
			"Static description of a device, always 1.",
			[]string{labelDevice, labelNode, "mac", "broadcast"}, nil),
		state: prometheus.NewDesc(
			Namespace+"_device_power_state",
			"Observed power state, 1 on the state that applies. States: Unknown, Pending, Starting, Running, Stopping, Stopped, Failed.",
			[]string{labelDevice, labelNode, "state"}, nil),
		desired: prometheus.NewDesc(
			Namespace+"_device_desired_power_state",
			"Requested power state, 1 on the state that applies. States: Running, Stopped, Unmanaged.",
			[]string{labelDevice, labelNode, "state"}, nil),
		paused: prometheus.NewDesc(
			Namespace+"_device_paused",
			"1 when the device is paused and sandman will not act on it.",
			deviceLabels, nil),
		nodeFound: prometheus.NewDesc(
			Namespace+"_device_node_found",
			"1 when the referenced Node object exists. A stopped machine usually has none.",
			deviceLabels, nil),
		nodeReady: prometheus.NewDesc(
			Namespace+"_device_node_ready",
			"1 when the node's Ready condition is true. Absent while there is no node.",
			deviceLabels, nil),
		unschedulable: prometheus.NewDesc(
			Namespace+"_device_node_unschedulable",
			"1 when the node is cordoned. Absent while there is no node.",
			deviceLabels, nil),
		phaseSince: prometheus.NewDesc(
			Namespace+"_device_phase_since_timestamp_seconds",
			"When the current power state was entered.",
			deviceLabels, nil),
		wakeAttempts: prometheus.NewDesc(
			Namespace+"_device_wake_attempts",
			"Magic packets sent during the start in flight. Resets when the start settles.",
			deviceLabels, nil),
		lastWake: prometheus.NewDesc(
			Namespace+"_device_last_wake_timestamp_seconds",
			"When the last magic packet was sent.",
			deviceLabels, nil),
		lastShutdown: prometheus.NewDesc(
			Namespace+"_device_last_shutdown_timestamp_seconds",
			"When the last shutdown was issued.",
			deviceLabels, nil),
		lastStarted: prometheus.NewDesc(
			Namespace+"_device_last_started_timestamp_seconds",
			"When the machine last reached Started.",
			deviceLabels, nil),
		lastStopped: prometheus.NewDesc(
			Namespace+"_device_last_stopped_timestamp_seconds",
			"When the machine last reached Stopped.",
			deviceLabels, nil),
		transitionUntil: prometheus.NewDesc(
			Namespace+"_device_transition_deadline_timestamp_seconds",
			"When the transition in flight is declared failed. Absent when nothing is in flight, which is what makes it usable as an alert threshold.",
			deviceLabels, nil),
		scrapeError: prometheus.NewDesc(
			Namespace+"_device_collect_failures_total",
			"1 when the last scrape could not read the device list.",
			nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *DeviceCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.info, c.state, c.desired, c.paused, c.nodeFound, c.nodeReady,
		c.unschedulable, c.phaseSince, c.wakeAttempts, c.lastWake,
		c.lastShutdown, c.lastStarted, c.lastStopped, c.transitionUntil,
		c.scrapeError,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *DeviceCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	devices, err := c.list(ctx)
	if err != nil {
		// Reporting the failure beats reporting nothing: a scrape that
		// silently loses every device series looks exactly like a cluster with
		// no devices in it.
		c.logger.Error("collecting device metrics failed", "error", err)
		ch <- prometheus.MustNewConstMetric(c.scrapeError, prometheus.GaugeValue, 1)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.scrapeError, prometheus.GaugeValue, 0)

	for i := range devices {
		c.collectDevice(ch, &devices[i])
	}
}

func (c *DeviceCollector) collectDevice(ch chan<- prometheus.Metric, device *v1alpha1.SandmanDevice) {
	name := device.Name
	node := device.Spec.NodeRef.Name
	status := &device.Status

	gauge := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, append([]string{name, node}, labels...)...)
	}

	gauge(c.info, 1, device.Spec.WakeOnLAN.MACAddress, device.Spec.WakeOnLAN.BroadcastAddr())

	phase := status.Phase
	if phase == "" {
		phase = v1alpha1.PowerPhaseUnknown
	}
	for _, p := range allPhases {
		gauge(c.state, boolValue(p == phase), string(p))
	}
	for _, s := range allStates {
		gauge(c.desired, boolValue(s == device.Spec.Desired()), string(s))
	}

	gauge(c.paused, boolValue(device.Spec.Paused))
	gauge(c.nodeFound, boolValue(status.NodeFound))
	if status.NodeReady != nil {
		gauge(c.nodeReady, boolValue(*status.NodeReady))
	}
	if status.Unschedulable != nil {
		gauge(c.unschedulable, boolValue(*status.Unschedulable))
	}
	gauge(c.wakeAttempts, float64(status.WakeAttempts))

	timestamp(ch, c.phaseSince, status.PhaseSince, name, node)
	timestamp(ch, c.lastWake, status.LastWakeTime, name, node)
	timestamp(ch, c.lastShutdown, status.LastShutdownTime, name, node)
	timestamp(ch, c.lastStarted, status.LastStartedTime, name, node)
	timestamp(ch, c.lastStopped, status.LastStoppedTime, name, node)

	if phase.InFlight() && status.PhaseSince != nil {
		budget := device.Spec.Timeouts.StartTimeout()
		if phase == v1alpha1.PowerPhaseStopping {
			budget = device.Spec.Timeouts.StopTimeout()
		}
		gauge(c.transitionUntil, float64(status.PhaseSince.Add(budget).Unix()))
	}
}

func timestamp(ch chan<- prometheus.Metric, desc *prometheus.Desc, t *metav1.Time, name, node string) {
	if t == nil || t.IsZero() {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(t.Unix()), name, node)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
