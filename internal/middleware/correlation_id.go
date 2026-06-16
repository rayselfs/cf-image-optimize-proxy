package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/requestid"
)

// CorrelationID is a middleware that reads or generates a request correlation ID.
// It reads X-Request-Id from the incoming request; if absent or invalid, generates a random
// 16-byte hex string. The ID is stored in the request context, set on the response
// header, and available via requestid.FromContext.
func CorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || !isValidRequestID(id) {
			id = generateID()
		}

		ctx := requestid.WithContext(r.Context(), id)
		w.Header().Set("X-Request-Id", id)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// generateID returns a random 32-character hex string (16 bytes).
func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use a fixed placeholder (should never happen in practice).
		return "0000000000000000000000000000000000000000000000000000000000000000"[:32]
	}
	return hex.EncodeToString(b)
}

// isValidRequestID checks if the request ID is valid:
// - max length 128 chars
// - only alphanumeric, hyphens, underscores
func isValidRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
