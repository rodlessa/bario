// Package chirphttp holds the HTTP plumbing shared by all Chirp services:
// routing with canonical metric labels, JSON helpers, auth middleware and
// server lifecycle.
package chirphttp

import (
	"net/http"

	"chirp/pkg/metrics"
)

// Router registers routes together with the canonical endpoint label used for
// metrics. Services mount the same handlers under the bare path (direct access)
// and under their public /api/<service> prefix (via the Ingress).
type Router struct {
	mux     *http.ServeMux
	metrics *metrics.Metrics
}

// NewRouter returns a router that instruments every registered route.
func NewRouter(m *metrics.Metrics) *Router {
	return &Router{mux: http.NewServeMux(), metrics: m}
}

// Handle registers method + path. endpoint must be a stable, low-cardinality
// name such as "create_post"; it is never the raw path.
func (r *Router) Handle(method, path, endpoint string, handler http.HandlerFunc) {
	pattern := method + " " + path
	r.mux.Handle(pattern, r.metrics.Instrument(endpoint, handler))
}

// Get registers a GET route.
func (r *Router) Get(path, endpoint string, handler http.HandlerFunc) {
	r.Handle(http.MethodGet, path, endpoint, handler)
}

// Post registers a POST route.
func (r *Router) Post(path, endpoint string, handler http.HandlerFunc) {
	r.Handle(http.MethodPost, path, endpoint, handler)
}

// Delete registers a DELETE route.
func (r *Router) Delete(path, endpoint string, handler http.HandlerFunc) {
	r.Handle(http.MethodDelete, path, endpoint, handler)
}

// Handler returns the underlying HTTP handler.
func (r *Router) Handler() http.Handler {
	return r.mux
}
