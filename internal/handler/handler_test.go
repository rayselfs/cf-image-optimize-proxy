package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/cache"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/imgproxy"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/upstream"
)

type mockCache struct {
	getBody        []byte
	getContentType string
	getErr         error
	getCalls       int
	putCalls       int
	putKey         string
	putBody        []byte
	putContentType string
	putErr         error
}

func (m *mockCache) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	m.getCalls++
	if m.getErr != nil {
		return nil, "", m.getErr
	}
	return io.NopCloser(bytes.NewReader(m.getBody)), m.getContentType, nil
}

func (m *mockCache) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	m.putCalls++
	m.putKey = key
	m.putContentType = contentType
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if m.putErr != nil {
		return m.putErr
	}
	m.putBody = data
	m.getBody = data
	m.getContentType = contentType
	m.getErr = nil
	return nil
}

type mockTransformer struct {
	body        []byte
	contentType string
	err         error
	calls       int
	sourceURL   string
	params      imgproxy.TransformParams
}

func (m *mockTransformer) Transform(ctx context.Context, sourceURL string, params imgproxy.TransformParams) (io.ReadCloser, string, error) {
	m.calls++
	m.sourceURL = sourceURL
	m.params = params
	if m.err != nil {
		return nil, "", m.err
	}
	return io.NopCloser(bytes.NewReader(m.body)), m.contentType, nil
}

type mockResolver struct {
	sourceURL       string
	body            []byte
	contentType     string
	headContentType string
	err             error
	fetchErr        error
	headErr         error
	calls           int
	fetchCalls      int
	headCalls       int
}

func (m *mockResolver) Resolve(r *http.Request) (string, func() (string, error), func() (io.ReadCloser, string, error), error) {
	m.calls++
	if m.err != nil {
		return "", nil, nil, m.err
	}
	headCT := m.headContentType
	if headCT == "" {
		headCT = m.contentType
	}
	headFunc := func() (string, error) {
		m.headCalls++
		if m.headErr != nil {
			return "", m.headErr
		}
		return headCT, nil
	}
	fetchFunc := func() (io.ReadCloser, string, error) {
		m.fetchCalls++
		if m.fetchErr != nil {
			return nil, "", m.fetchErr
		}
		return io.NopCloser(bytes.NewReader(m.body)), m.contentType, nil
	}
	return m.sourceURL, headFunc, fetchFunc, nil
}

type mockCoalescer struct {
	calls int
	key   string
}

func (m *mockCoalescer) Do(ctx context.Context, key string, fn func() (interface{}, error)) (interface{}, error, bool) {
	m.calls++
	m.key = key
	result, err := fn()
	return result, err, false
}

func TestParseParams(t *testing.T) {
	tests := map[string]struct {
		query url.Values
		want  *ImageParams
	}{
		"no optimize params": {
			query: url.Values{},
		},
		"missing format": {
			query: url.Values{"imwidth": {"640"}},
		},
		"invalid width": {
			query: url.Values{"imwidth": {"0"}, "f": {"webp"}},
		},
		"unsupported format": {
			query: url.Values{"imwidth": {"640"}, "f": {"png"}},
		},
		"default quality": {
			query: url.Values{"imwidth": {"640"}, "f": {"webp"}},
			want:  &ImageParams{Width: 640, Format: "webp", Quality: 75},
		},
		"clamps low quality": {
			query: url.Values{"imwidth": {"640"}, "f": {"avif"}, "q": {"0"}},
			want:  &ImageParams{Width: 640, Format: "avif", Quality: 1},
		},
		"clamps high quality": {
			query: url.Values{"imwidth": {"640"}, "f": {"jpeg"}, "q": {"101"}},
			want:  &ImageParams{Width: 640, Format: "jpeg", Quality: 100},
		},
		"invalid quality non-numeric": {
			query: url.Values{"imwidth": {"640"}, "f": {"webp"}, "q": {"abc"}},
			want:  &ImageParams{Width: 640, Format: "webp", Quality: 75},
		},
		"invalid quality negative": {
			query: url.Values{"imwidth": {"640"}, "f": {"webp"}, "q": {"-1"}},
			want:  &ImageParams{Width: 640, Format: "webp", Quality: 1},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := ParseParams(tt.query, 75)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("ParseParams() = %#v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseParams() = nil, want %#v", tt.want)
			}
			if *got != *tt.want {
				t.Fatalf("ParseParams() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPassThrough(t *testing.T) {
	c := &mockCache{}
	tx := &mockTransformer{}
	r := &mockResolver{body: []byte("original"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != "original" {
		t.Fatalf("body = %q, want %q", got, "original")
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if c.getCalls != 0 || c.putCalls != 0 {
		t.Fatalf("cache calls get=%d put=%d, want none", c.getCalls, c.putCalls)
	}
	if tx.calls != 0 {
		t.Fatalf("transform calls = %d, want 0", tx.calls)
	}
	if coal.calls != 0 {
		t.Fatalf("coalescer calls = %d, want 0", coal.calls)
	}
	if r.calls != 1 || r.fetchCalls != 1 || r.headCalls != 0 {
		t.Fatalf("resolver calls=%d fetch=%d head=%d, want 1/1/0", r.calls, r.fetchCalls, r.headCalls)
	}
}

func TestCacheHit(t *testing.T) {
	c := &mockCache{getBody: []byte("cached"), getContentType: "image/webp"}
	tx := &mockTransformer{}
	r := &mockResolver{}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if got := w.Body.String(); got != "cached" {
		t.Fatalf("body = %q, want cached", got)
	}
	if got := w.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", got)
	}
	if coal.calls != 0 {
		t.Fatalf("coalescer calls = %d, want 0 (cache HIT bypasses coalescer)", coal.calls)
	}
	if r.calls != 0 || tx.calls != 0 || c.putCalls != 0 {
		t.Fatalf("unexpected calls resolver=%d transform=%d put=%d", r.calls, tx.calls, c.putCalls)
	}
}

func TestCacheMissTransform(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{body: []byte("transformed"), contentType: "image/avif"}
	r := &mockResolver{sourceURL: "https://origin/image.png", body: []byte("original"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=avif&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if got := w.Body.String(); got != "transformed" {
		t.Fatalf("body = %q, want transformed", got)
	}
	if got := w.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", got)
	}
	if c.putCalls != 1 || c.putKey != "example.com/image.png/640_avif_80" {
		t.Fatalf("cache put calls=%d key=%q", c.putCalls, c.putKey)
	}
	if tx.calls != 1 || tx.sourceURL != "https://origin/image.png" {
		t.Fatalf("transform calls=%d source=%q", tx.calls, tx.sourceURL)
	}
	if r.calls != 1 || r.fetchCalls != 0 || r.headCalls != 1 {
		t.Fatalf("resolver calls=%d fetch=%d head=%d, want 1/0/1", r.calls, r.fetchCalls, r.headCalls)
	}
}

func TestCacheMissTransformNoFetch(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{body: []byte("transformed"), contentType: "image/avif"}
	r := &mockResolver{
		sourceURL:       "https://origin/image.png",
		contentType:     "image/png",
		headContentType: "image/png",
	}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/img.png?imwidth=640&f=avif&q=80", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if r.fetchCalls != 0 {
		t.Fatalf("fetchCalls = %d, want 0 (successful transform must not fetch original)", r.fetchCalls)
	}
	if r.headCalls != 1 {
		t.Fatalf("headCalls = %d, want 1", r.headCalls)
	}
	if c.putCalls != 1 {
		t.Fatalf("putCalls = %d, want 1", c.putCalls)
	}
}

func TestImgproxyFailure(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{err: errors.New("imgproxy down")}
	r := &mockResolver{sourceURL: "https://origin/image.png", body: []byte("original"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if got := w.Body.String(); got != "original" {
		t.Fatalf("body = %q, want original", got)
	}
	if got := w.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", got)
	}
	if r.fetchCalls != 1 || r.headCalls != 1 {
		t.Fatalf("fetch calls = %d head calls = %d, want 1/1", r.fetchCalls, r.headCalls)
	}
	if c.putCalls != 0 {
		t.Fatalf("cache put calls = %d, want 0", c.putCalls)
	}
}

func TestNonImagePassThrough(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{}
	r := &mockResolver{sourceURL: "https://origin/page", body: []byte("<html></html>"), contentType: "text/html", headContentType: "text/html"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/page?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if got := w.Body.String(); got != "<html></html>" {
		t.Fatalf("body = %q, want html", got)
	}
	if got := w.Header().Get("X-Cache"); got != "BYPASS" {
		t.Fatalf("X-Cache = %q, want BYPASS", got)
	}
	if tx.calls != 0 || c.putCalls != 0 {
		t.Fatalf("unexpected transform=%d put=%d", tx.calls, c.putCalls)
	}
}

func TestMaxWidthCap(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{body: []byte("transformed"), contentType: "image/webp"}
	r := &mockResolver{sourceURL: "https://origin/image.png", body: []byte("original"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=3000&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if got := coal.key; got != "example.com/image.png/1920_webp_80" {
		t.Fatalf("coalescer key = %q", got)
	}
	if tx.params.Width != 1920 {
		t.Fatalf("transform width = %d, want 1920", tx.params.Width)
	}
}

func TestPassThroughResolverError(t *testing.T) {
	c := &mockCache{}
	tx := &mockTransformer{}
	r := &mockResolver{err: errors.New("resolver down")}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	// No transform params → triggers pass-through path
	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestPassThroughFetchError(t *testing.T) {
	c := &mockCache{}
	tx := &mockTransformer{}
	r := &mockResolver{fetchErr: errors.New("upstream unreachable"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestProcessResolverError(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{}
	r := &mockResolver{err: errors.New("resolve failed")}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestProcessFetchError(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{}
	r := &mockResolver{fetchErr: errors.New("upstream unreachable"), sourceURL: "https://origin/image.png", contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestCachePutError(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound, putErr: errors.New("s3 write error")}
	tx := &mockTransformer{body: []byte("transformed"), contentType: "image/webp"}
	r := &mockResolver{sourceURL: "https://origin/image.png", body: []byte("original"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (cache Put error is fatal)", w.Code)
	}
	if c.putCalls != 1 {
		t.Fatalf("putCalls = %d, want 1", c.putCalls)
	}
}

func TestPassThroughUnderLimit(t *testing.T) {
	c := &mockCache{}
	tx := &mockTransformer{}
	r := &mockResolver{body: []byte("12345678"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != "12345678" {
		t.Fatalf("body = %q, want %q", got, "12345678")
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
}

func TestPassThroughOverLimit(t *testing.T) {
	// Pass-through should stream successfully even if the file size exceeds MaxBodyBytes
	c := &mockCache{}
	tx := &mockTransformer{}
	r := &mockResolver{body: []byte("original body"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != "original body" {
		t.Fatalf("body = %q, want %q", got, "original body")
	}
}

func TestPassThroughCRLFContentType(t *testing.T) {
	// CR/LF in content type must be rejected (no header injection).
	c := &mockCache{}
	tx := &mockTransformer{}
	r := &mockResolver{body: []byte("data"), contentType: "image/png\r\nX-Injected: evil"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (CRLF content type must be rejected)", w.Code, http.StatusBadGateway)
	}
	if got := w.Header().Get("X-Injected"); got != "" {
		t.Fatalf("X-Injected header leaked: %q", got)
	}
}

func TestTransformNonImageContentType(t *testing.T) {
	// imgproxy returning non-image content type must fall back to original.
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{body: []byte("not-an-image"), contentType: "text/html"}
	r := &mockResolver{sourceURL: "https://origin/img.png", body: []byte("original"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/img.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "original" {
		t.Fatalf("body = %q, want original", got)
	}
	if c.putCalls != 0 {
		t.Fatalf("cache.Put should not be called on invalid transform content type, got %d calls", c.putCalls)
	}
}

func TestCacheGetNonMissError(t *testing.T) {
	c := &mockCache{getErr: errors.New("s3 read error")}
	tx := &mockTransformer{body: []byte("transformed"), contentType: "image/webp"}
	r := &mockResolver{sourceURL: "https://origin/image.png", body: []byte("original"), contentType: "image/png"}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (initial cache error logs and falls through; put+get succeeds)", w.Code)
	}
	if got := w.Body.String(); got != "transformed" {
		t.Fatalf("body = %q, want transformed", got)
	}
}

func TestStatusForwarding(t *testing.T) {
	c := &mockCache{getErr: cache.ErrNotFound}
	tx := &mockTransformer{}
	r := &mockResolver{
		err: &upstream.StatusError{Code: http.StatusNotFound},
	}
	coal := &mockCoalescer{}
	h := New(c, tx, r, coal, 1920, 75)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.png?imwidth=640&f=webp&q=80", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	// Test 403
	r.err = &upstream.StatusError{Code: http.StatusForbidden}
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w2.Code, http.StatusForbidden)
	}

	// Test 500 (maps to 502)
	r.err = &upstream.StatusError{Code: http.StatusInternalServerError}
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req)
	if w3.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w3.Code, http.StatusBadGateway)
	}
}
