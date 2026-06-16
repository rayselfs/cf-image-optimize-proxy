package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/metrics"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/requestid"
)

// Logging is a structured JSON logging middleware.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(wrapped, r)

		metrics.IncRequest()

		cacheStatus := wrapped.Header().Get("X-Cache")
		if cacheStatus == "" {
			cacheStatus = "UNKNOWN"
		}
		metrics.ObserveHTTPRequest(r.Method, httpStatusClass(wrapped.statusCode), cacheStatus, time.Since(start).Seconds())

		slog.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"x_cache", wrapped.Header().Get("X-Cache"),
			"bytes_written", wrapped.bytesWritten,
			"remote_addr", r.RemoteAddr,
			"request_id", requestid.FromContext(r.Context()),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (w *responseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytesWritten += n
	return n, err
}

func httpStatusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "other"
	}
}
