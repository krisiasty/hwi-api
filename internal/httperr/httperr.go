// Package httperr writes the one JSON error body shape shared by every
// non-2xx response this agent serves, over HTTP.
//
// It exists because internal/api and internal/server/middleware.go each
// needed to write that same shape and neither may depend on the other:
// internal/server is transport and internal/api is handlers, and main.go
// composes the latter on top of the former, so a dependency in either
// direction would invert that layering. apiv1 was ruled out too - its own
// doc comment promises it contains types and constants only, specifically
// so that importing it commits a consumer to nothing else; adding a
// net/http dependency there would break that promise for every existing
// importer. This package is the shared leaf both call instead: it imports
// apiv1 for the wire shape, and nothing imports it back.
package httperr

import (
	"encoding/json"
	"net/http"

	"github.com/krisiasty/hwi-api/apiv1"
)

// Write writes a JSON apiv1.ErrorResponse body with status, code and
// message, setting the headers every error response needs:
// Content-Type, Cache-Control: no-store (error bodies are never cached, so
// a stale failure is never replayed to a later, possibly successful,
// request) and X-Content-Type-Options: nosniff.
//
// The encode error is deliberately discarded: by the time Encode runs,
// WriteHeader has already committed the status code, so there is nothing
// left to do with a failure here beyond what net/http already does on a
// broken connection.
func Write(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiv1.ErrorResponse{Error: apiv1.Error{Code: code, Message: message}})
}
