package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hauke-cloud/sandman/internal/metrics"
)

// readHeaderTimeout bounds how long a client may take to send its request
// headers, which is what keeps a slow-loris client from holding a connection.
const readHeaderTimeout = 10 * time.Second

// maxBodyBytes caps a request body. The bodies here are two short strings, so
// anything larger is a mistake or an attack.
const maxBodyBytes = 32 << 10

// Options configures the HTTP server.
type Options struct {
	// Address is the listen address.
	Address string
	// MetricsPath is where the Prometheus handler is mounted.
	MetricsPath string
	// Gatherer supplies the metrics.
	Gatherer prometheus.Gatherer
	// Client reads and patches SandmanDevices. Reads go through the
	// controller's cache, so listing the fleet costs nothing.
	Client client.Client
	// Ready reports whether the cache has synced. Until it has, the API would
	// answer from an empty cache, which reads as "no devices" rather than as
	// "not ready" -- a difference worth having a probe for.
	Ready func() bool
	// Token, when set, is required as a bearer token on every API route.
	Token string
	// ReadOnly refuses every state-changing route. Useful for an instance
	// exposed more widely than the one that does the work.
	ReadOnly bool
	// Metrics records API traffic. May be nil.
	Metrics *metrics.Metrics
	// Logger receives handler errors.
	Logger *slog.Logger
}

// Server is sandman's HTTP surface.
type Server struct {
	opts    *Options
	logger  *slog.Logger
	handler http.Handler
}

// New builds the server. It does not listen; call Start or use HTTPServer.
func New(opts *Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{opts: opts, logger: logger}
	s.handler = s.routes()
	return s
}

// Handler returns the routed handler, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.handler }

// HTTPServer returns a configured http.Server.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.opts.Address,
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelError),
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Probes and metrics are deliberately outside the API's authentication:
	// the kubelet has no token to present, and neither does Prometheus.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writePlain(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.opts.Ready == nil || s.opts.Ready() {
			writePlain(w, http.StatusOK, "ok")
			return
		}
		writePlain(w, http.StatusServiceUnavailable, "device cache has not synced yet")
	})
	if s.opts.Gatherer != nil {
		mux.Handle("GET "+s.opts.MetricsPath, promhttp.HandlerFor(s.opts.Gatherer, promhttp.HandlerOpts{
			ErrorLog:            slog.NewLogLogger(s.logger.Handler(), slog.LevelError),
			ErrorHandling:       promhttp.ContinueOnError,
			MaxRequestsInFlight: 4,
		}))
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeHTML(w, http.StatusOK, landingPage(s.opts.MetricsPath))
	})

	api := map[string]http.HandlerFunc{
		"GET /api/v1/devices":                 s.listDevices,
		"GET /api/v1/devices/{name}":          s.getDevice,
		"GET /api/v1/devices/{name}/status":   s.getDeviceStatus,
		"POST /api/v1/devices/{name}/start":   s.startDevice,
		"POST /api/v1/devices/{name}/stop":    s.stopDevice,
		"POST /api/v1/devices/{name}/release": s.releaseDevice,
		// Everything is also addressable by node name, because an operator
		// looking at a cluster thinks in nodes, and the device that manages a
		// node is not necessarily named after it.
		"GET /api/v1/nodes":               s.listDevices,
		"GET /api/v1/nodes/{node}":        s.getNode,
		"GET /api/v1/nodes/{node}/status": s.getNodeStatus,
		"POST /api/v1/nodes/{node}/start": s.startNode,
		"POST /api/v1/nodes/{node}/stop":  s.stopNode,
	}
	for pattern, handler := range api {
		mux.Handle(pattern, s.instrument(pattern, s.authenticate(handler)))
	}

	return mux
}

// authenticate enforces the bearer token, when one is configured.
func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token == "" {
			next(w, r)
			return
		}
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandman"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "a bearer token is required", "")
			return
		}
		// Constant time: the comparison is against a secret, and a timing
		// oracle on a token that turns machines off is not a subtle problem.
		if subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(s.opts.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token", "")
			return
		}
		next(w, r)
	}
}

// instrument records latency and outcome per route. The route pattern is the
// label rather than the path, so a fleet of a thousand machines produces one
// series per route instead of a thousand.
func (s *Server) instrument(route string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(recorder, r)

		if s.opts.Metrics == nil {
			return
		}
		s.opts.Metrics.APIRequestsTotal.
			WithLabelValues(route, r.Method, strconv.Itoa(recorder.status)).Inc()
		s.opts.Metrics.APIRequestDurationSeconds.
			WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush lets the embedded writer's flushing survive the wrapper, which the
// Prometheus handler and any streaming response rely on.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message, details string) {
	writeJSON(w, status, Error{Error: code, Message: message, Details: details})
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, body)
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body)
}

// decodeBody reads an optional JSON body. An empty body is valid: every field
// of a power request is optional.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func landingPage(metricsPath string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>sandman</title></head>
<body>
<h1>sandman</h1>
<p>Wake-on-LAN and graceful shutdown for Kubernetes nodes.</p>
<ul>
<li><code>GET /api/v1/devices</code> &mdash; every device and its power state</li>
<li><code>GET /api/v1/devices/{name}</code> &mdash; one device</li>
<li><code>POST /api/v1/devices/{name}/start</code> &mdash; wake the machine</li>
<li><code>POST /api/v1/devices/{name}/stop</code> &mdash; drain and power the machine off</li>
<li><code>POST /api/v1/devices/{name}/release</code> &mdash; stop managing the machine</li>
<li><code>GET /api/v1/nodes/{node}</code> &mdash; the same, addressed by node name</li>
<li><a href="%s">Metrics</a></li>
<li><a href="/healthz">Liveness</a> &middot; <a href="/readyz">Readiness</a></li>
</ul>
</body>
</html>
`, metricsPath)
}
