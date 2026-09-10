package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
)

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("sandman scheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.SandmanDevice{}).
		WithObjects(objects...).
		Build()
}

func device(name, node string, mutators ...func(*v1alpha1.SandmanDevice)) *v1alpha1.SandmanDevice {
	d := &v1alpha1.SandmanDevice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SandmanDeviceSpec{
			NodeRef:   v1alpha1.NodeReference{Name: node},
			WakeOnLAN: v1alpha1.WakeOnLANSpec{MACAddress: "aa:bb:cc:dd:ee:ff"},
		},
		Status: v1alpha1.SandmanDeviceStatus{Phase: v1alpha1.PowerPhaseRunning},
	}
	for _, mutate := range mutators {
		mutate(d)
	}
	return d
}

func newServer(t *testing.T, opts *Options) *Server {
	t.Helper()
	if opts.Client == nil {
		opts.Client = testClient(t)
	}
	if opts.MetricsPath == "" {
		opts.MetricsPath = "/metrics"
	}
	return New(opts)
}

func do(t *testing.T, s *Server, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return out
}

func TestListDevicesIsSortedAndSummarised(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t,
		device("worker-02", "worker-02"),
		device("worker-01", "worker-01", func(d *v1alpha1.SandmanDevice) {
			d.Status.Phase = v1alpha1.PowerPhaseStopped
		}),
	)})

	recorder := do(t, s, http.MethodGet, "/api/v1/devices", nil, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	list := decode[DeviceList](t, recorder)
	if list.Count != 2 {
		t.Fatalf("count = %d, want 2", list.Count)
	}
	if list.Items[0].Name != "worker-01" {
		t.Fatalf("first item = %q, want the list sorted by name", list.Items[0].Name)
	}
	if list.Summary[v1alpha1.PowerPhaseStopped] != 1 || list.Summary[v1alpha1.PowerPhaseRunning] != 1 {
		t.Fatalf("summary = %v, want one of each state", list.Summary)
	}
}

func TestGetDevice(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t, device("worker-01", "worker-01"))})

	recorder := do(t, s, http.MethodGet, "/api/v1/devices/worker-01", nil, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	got := decode[Device](t, recorder)
	if got.Name != "worker-01" || got.State != v1alpha1.PowerPhaseRunning {
		t.Fatalf("device = %+v", got)
	}
	if got.DesiredState != v1alpha1.PowerStateUnmanaged {
		t.Fatalf("desiredState = %q, want Unmanaged for a device nobody has asked anything of", got.DesiredState)
	}
}

func TestGetMissingDeviceIs404(t *testing.T) {
	s := newServer(t, &Options{})

	recorder := do(t, s, http.MethodGet, "/api/v1/devices/nope", nil, nil)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if got := decode[Error](t, recorder); got.Error != "not_found" {
		t.Fatalf("error code = %q", got.Error)
	}
}

func TestStartRecordsTheRequestAndReturnsImmediately(t *testing.T) {
	c := testClient(t, device("worker-01", "worker-01", func(d *v1alpha1.SandmanDevice) {
		d.Status.Phase = v1alpha1.PowerPhaseStopped
	}))
	s := newServer(t, &Options{Client: c})

	recorder := do(t, s, http.MethodPost, "/api/v1/devices/worker-01/start",
		PowerRequest{RequestedBy: "hauke", Reason: "batch job"}, nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the API records the request, it does not wait for the machine", recorder.Code)
	}
	response := decode[PowerResponse](t, recorder)
	if !response.Accepted || response.RequestID == "" {
		t.Fatalf("response = %+v", response)
	}

	var stored v1alpha1.SandmanDevice
	if err := c.Get(context.Background(), types.NamespacedName{Name: "worker-01"}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.DesiredPowerState != v1alpha1.PowerStateRunning {
		t.Fatalf("desiredPowerState = %q, want On", stored.Spec.DesiredPowerState)
	}
	if stored.Annotations[v1alpha1.AnnotationRequestID] != response.RequestID {
		t.Fatal("the request id must be recorded on the device, so a retry is distinguishable from a repeat")
	}
	if stored.Annotations[v1alpha1.AnnotationRequestedBy] != "hauke" {
		t.Fatalf("requestedBy = %q", stored.Annotations[v1alpha1.AnnotationRequestedBy])
	}
}

func TestStopRecordsTheRequest(t *testing.T) {
	c := testClient(t, device("worker-01", "worker-01"))
	s := newServer(t, &Options{Client: c})

	recorder := do(t, s, http.MethodPost, "/api/v1/devices/worker-01/stop", nil, nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}
	var stored v1alpha1.SandmanDevice
	if err := c.Get(context.Background(), types.NamespacedName{Name: "worker-01"}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.DesiredPowerState != v1alpha1.PowerStateStopped {
		t.Fatalf("desiredPowerState = %q, want Off", stored.Spec.DesiredPowerState)
	}
}

func TestStopIsRefusedWhenShutdownIsDisabled(t *testing.T) {
	c := testClient(t, device("worker-01", "worker-01", func(d *v1alpha1.SandmanDevice) {
		d.Spec.Shutdown.Mode = v1alpha1.ShutdownModeDisabled
	}))
	s := newServer(t, &Options{Client: c})

	recorder := do(t, s, http.MethodPost, "/api/v1/devices/worker-01/stop", nil, nil)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	var stored v1alpha1.SandmanDevice
	if err := c.Get(context.Background(), types.NamespacedName{Name: "worker-01"}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.DesiredPowerState == v1alpha1.PowerStateStopped {
		t.Fatal("a refused request must not be written to the device")
	}
}

func TestReleaseStopsManagingTheDevice(t *testing.T) {
	c := testClient(t, device("worker-01", "worker-01", func(d *v1alpha1.SandmanDevice) {
		d.Spec.DesiredPowerState = v1alpha1.PowerStateRunning
	}))
	s := newServer(t, &Options{Client: c})

	if recorder := do(t, s, http.MethodPost, "/api/v1/devices/worker-01/release", nil, nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}

	var stored v1alpha1.SandmanDevice
	if err := c.Get(context.Background(), types.NamespacedName{Name: "worker-01"}, &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.DesiredPowerState != v1alpha1.PowerStateUnmanaged {
		t.Fatalf("desiredPowerState = %q, want Unmanaged", stored.Spec.DesiredPowerState)
	}
}

func TestNodeRoutesResolveThroughTheNodeReference(t *testing.T) {
	c := testClient(t, device("basement-box", "worker-01"))
	s := newServer(t, &Options{Client: c})

	recorder := do(t, s, http.MethodGet, "/api/v1/nodes/worker-01", nil, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := decode[Device](t, recorder); got.Name != "basement-box" {
		t.Fatalf("device = %q, want the device managing the node whatever it is called", got.Name)
	}

	if recorder := do(t, s, http.MethodPost, "/api/v1/nodes/worker-01/start", nil, nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("start by node: status = %d, want 202", recorder.Code)
	}
}

func TestTwoDevicesForOneNodeIsReportedRatherThanGuessed(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t,
		device("a", "worker-01"),
		device("b", "worker-01"),
	)})

	recorder := do(t, s, http.MethodGet, "/api/v1/nodes/worker-01", nil, nil)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	got := decode[Error](t, recorder)
	if got.Error != "ambiguous_node" || !strings.Contains(got.Details, "a") {
		t.Fatalf("error = %+v, want both candidates named", got)
	}
}

func TestUnknownNodeIs404(t *testing.T) {
	s := newServer(t, &Options{})

	if recorder := do(t, s, http.MethodGet, "/api/v1/nodes/ghost", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestStatusEndpointIsSmall(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t, device("worker-01", "worker-01"))})

	recorder := do(t, s, http.MethodGet, "/api/v1/devices/worker-01/status", nil, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	got := decode[map[string]any](t, recorder)
	if got["state"] != string(v1alpha1.PowerPhaseRunning) {
		t.Fatalf("state = %v", got["state"])
	}
	if _, present := got["conditions"]; present {
		t.Fatal("the status endpoint exists to be small; it should not carry conditions")
	}
}

func TestBearerTokenIsEnforcedOnApiRoutes(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t, device("worker-01", "worker-01")), Token: "s3cret"})

	if recorder := do(t, s, http.MethodGet, "/api/v1/devices", nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without a token = %d, want 401", recorder.Code)
	}
	if recorder := do(t, s, http.MethodGet, "/api/v1/devices", nil,
		map[string]string{"Authorization": "Bearer wrong"}); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status with the wrong token = %d, want 401", recorder.Code)
	}
	if recorder := do(t, s, http.MethodGet, "/api/v1/devices", nil,
		map[string]string{"Authorization": "Bearer s3cret"}); recorder.Code != http.StatusOK {
		t.Fatalf("status with the right token = %d, want 200", recorder.Code)
	}
}

// The kubelet and Prometheus have no token to present, so the probes and the
// metrics endpoint stay outside the API's authentication.
func TestProbesAndMetricsDoNotRequireAToken(t *testing.T) {
	s := newServer(t, &Options{Token: "s3cret", Ready: func() bool { return true }})

	for _, path := range []string{"/healthz", "/readyz"} {
		if recorder := do(t, s, http.MethodGet, path, nil, nil); recorder.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, recorder.Code)
		}
	}
}

func TestReadinessFailsUntilTheCacheHasSynced(t *testing.T) {
	synced := false
	s := newServer(t, &Options{Ready: func() bool { return synced }})

	if recorder := do(t, s, http.MethodGet, "/readyz", nil, nil); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: an empty cache reads as an empty fleet", recorder.Code)
	}
	synced = true
	if recorder := do(t, s, http.MethodGet, "/readyz", nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestReadOnlyRefusesEveryWrite(t *testing.T) {
	c := testClient(t, device("worker-01", "worker-01"))
	s := newServer(t, &Options{Client: c, ReadOnly: true})

	for _, path := range []string{
		"/api/v1/devices/worker-01/start",
		"/api/v1/devices/worker-01/stop",
		"/api/v1/nodes/worker-01/stop",
	} {
		if recorder := do(t, s, http.MethodPost, path, nil, nil); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s = %d, want 403", path, recorder.Code)
		}
	}

	if recorder := do(t, s, http.MethodGet, "/api/v1/devices", nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("reads must keep working read-only, got %d", recorder.Code)
	}
}

func TestInvalidBodyIsRejected(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t, device("worker-01", "worker-01"))})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/devices/worker-01/start",
		strings.NewReader(`{"requestedBy": }`))
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestEmptyBodyIsAccepted(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t, device("worker-01", "worker-01"))})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/devices/worker-01/start", http.NoBody)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: every field of a power request is optional", recorder.Code)
	}
}

func TestLandingPageIsServed(t *testing.T) {
	s := newServer(t, &Options{})

	recorder := do(t, s, http.MethodGet, "/", nil, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "/api/v1/devices") {
		t.Fatal("the landing page should point at the API")
	}
}

func TestDeviceViewCarriesTheNodeStatus(t *testing.T) {
	ready := true
	s := newServer(t, &Options{Client: testClient(t, device("worker-01", "worker-01", func(d *v1alpha1.SandmanDevice) {
		d.Status.NodeFound = true
		d.Status.NodeReady = &ready
		d.Status.Phase = v1alpha1.PowerPhaseStarting
		d.Status.Conditions = []metav1.Condition{{
			Type:               v1alpha1.ConditionPowered,
			Status:             metav1.ConditionUnknown,
			Reason:             "Starting",
			LastTransitionTime: metav1.Now(),
		}}
	}))})

	got := decode[Device](t, do(t, s, http.MethodGet, "/api/v1/devices/worker-01", nil, nil))

	if !got.InFlight {
		t.Fatal("Starting is an in-flight state; a caller has to know to keep polling")
	}
	if !got.NodeStatus.Found || got.NodeStatus.Ready == nil || !*got.NodeStatus.Ready {
		t.Fatalf("nodeStatus = %+v", got.NodeStatus)
	}
	if len(got.Conditions) != 1 {
		t.Fatalf("conditions = %v, want the device's own passed through", got.Conditions)
	}
}

func TestPodsAreNotPartOfTheApiSurface(t *testing.T) {
	// A guard against the API growing a route that exposes the cluster rather
	// than the fleet.
	s := newServer(t, &Options{})

	if recorder := do(t, s, http.MethodGet, "/api/v1/pods", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestNodeStatusRouteResolvesTheDevice(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t, device("basement-box", "worker-01"))})

	recorder := do(t, s, http.MethodGet, "/api/v1/nodes/worker-01/status", nil, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	got := decode[map[string]any](t, recorder)
	if got["name"] != "basement-box" || got["node"] != "worker-01" {
		t.Fatalf("status = %v, want the device that manages the node", got)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	s := newServer(t, &Options{Client: testClient(t, device("worker-01", "worker-01"))})

	huge := `{"reason": "` + strings.Repeat("x", 64<<10) + `"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/devices/worker-01/start", strings.NewReader(huge))
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}
