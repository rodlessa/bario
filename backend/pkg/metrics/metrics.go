// Package metrics exposes the Prometheus instrumentation shared by every Bario
// service. Each service gets its own registry so that only its own metrics are
// served, and registers the two canonical Bario metrics.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns the registry and the metric vectors of a single service.
type Metrics struct {
	service  string
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// New creates the metric set for service (one of auth, posts, uploads, follows).
func New(service string) *Metrics {
	registry := prometheus.NewRegistry()

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "bario_http_requests_total",
		Help: "Total number of HTTP requests handled by a Bario service.",
	}, []string{"service", "endpoint", "method", "status"})

	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "bario_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"service", "endpoint", "method"})

	registry.MustRegister(requests, duration)
	registry.MustRegister(prometheus.NewGoCollector())

	return &Metrics{
		service:  service,
		registry: registry,
		requests: requests,
		duration: duration,
	}
}

// Handler returns the /metrics handler for the service registry.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	return mux
}

// Instrument labels a handler with its canonical endpoint name and records a
// request counter increment and a latency observation for every call.
func (m *Metrics) Instrument(endpoint string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		m.requests.WithLabelValues(m.service, endpoint, r.Method, strconv.Itoa(status)).Inc()
		m.duration.WithLabelValues(m.service, endpoint, r.Method).Observe(time.Since(start).Seconds())
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

// Unwrap keeps http.ResponseController features (flushing, deadlines) working
// through the recorder.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
