package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/requestid"
)

func TestCorrelationIDForwarded(t *testing.T) {
	const wantID = "test-123"

	var gotCtxID string
	handler := CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = requestid.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", wantID)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Request-Id"); got != wantID {
		t.Errorf("response X-Request-Id = %q, want %q", got, wantID)
	}
	if gotCtxID != wantID {
		t.Errorf("context request ID = %q, want %q", gotCtxID, wantID)
	}
}

func TestCorrelationIDGenerated(t *testing.T) {
	var gotCtxID string
	handler := CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = requestid.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	respID := rr.Header().Get("X-Request-Id")
	if len(respID) != 32 {
		t.Errorf("generated X-Request-Id length = %d, want 32 (16-byte hex); got %q", len(respID), respID)
	}
	if respID == "" {
		t.Error("expected non-empty generated X-Request-Id")
	}
	if gotCtxID != respID {
		t.Errorf("context ID %q != response header ID %q", gotCtxID, respID)
	}
}

func TestCorrelationIDInvalidIDRejected(t *testing.T) {
	var gotCtxID string
	handler := CorrelationID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = requestid.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "../../../../etc/passwd")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	respID := rr.Header().Get("X-Request-Id")
	if respID == "../../../../etc/passwd" {
		t.Error("invalid X-Request-Id was not rejected; got same value back")
	}
	if len(respID) != 32 {
		t.Errorf("generated X-Request-Id length = %d, want 32 (16-byte hex); got %q", len(respID), respID)
	}
	if gotCtxID != respID {
		t.Errorf("context ID %q != response header ID %q", gotCtxID, respID)
	}
}
