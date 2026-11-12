package bariohttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxJSONBody = 1 << 20 // 1 MiB

// WriteJSON writes payload as JSON with the given status code. A nil payload
// sends only headers, which is useful for 204 responses.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	// The response has already started; encoding failures cannot be reported.
	_ = json.NewEncoder(w).Encode(payload)
}

// WriteError writes the conventional {"error": "..."} JSON error body.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

// DecodeJSON decodes a single JSON object from the request body, rejecting
// oversized bodies and trailing data. It writes a 400 response and returns
// false on failure, so callers can simply return.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, "request body must contain a single JSON object")
		return false
	}
	return true
}

// Healthy is the handler used by Kubernetes probes.
func Healthy(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
