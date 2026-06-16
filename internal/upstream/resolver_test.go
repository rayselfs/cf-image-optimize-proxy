package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type mockPresigner struct {
	t            *testing.T
	bucket       string
	key          string
	response     string
	headResponse string
}

func (m mockPresigner) PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	m.t.Helper()
	if got := aws.ToString(params.Bucket); got != m.bucket {
		m.t.Fatalf("Bucket = %q, want %q", got, m.bucket)
	}
	if got := aws.ToString(params.Key); got != m.key {
		m.t.Fatalf("Key = %q, want %q", got, m.key)
	}
	return &v4.PresignedHTTPRequest{URL: m.response}, nil
}

func (m mockPresigner) PresignHeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	m.t.Helper()
	if got := aws.ToString(params.Bucket); got != m.bucket {
		m.t.Fatalf("Bucket = %q, want %q", got, m.bucket)
	}
	if got := aws.ToString(params.Key); got != m.key {
		m.t.Fatalf("Key = %q, want %q", got, m.key)
	}
	response := m.headResponse
	if response == "" {
		response = "https://s3.example.com/presigned-head"
	}
	return &v4.PresignedHTTPRequest{URL: response}, nil
}

func TestResolveS3(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("s3 image"))
	}))
	defer server.Close()

	originalNewS3Presigner := newS3Presigner
	newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
		return mockPresigner{
			t:            t,
			bucket:       "source-bucket",
			key:          "images/cat.png",
			response:     server.URL + "/presigned",
			headResponse: server.URL + "/presigned-head",
		}, nil
	}
	t.Cleanup(func() { newS3Presigner = originalNewS3Presigner })

	req := httptest.NewRequest(http.MethodGet, "http://origin.example/images/cat.png?width=640", nil)
	req.Header.Set("X-Img-Source-Type", "s3")
	req.Header.Set("X-Img-Source-Bucket", "source-bucket")

	sourceURL, headFunc, fetchFunc, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if sourceURL != server.URL+"/presigned" {
		t.Fatalf("sourceURL = %q, want presigned URL", sourceURL)
	}
	if headFunc == nil {
		t.Fatal("headFunc = nil, want function")
	}

	body, contentType, err := fetchFunc()
	if err != nil {
		t.Fatalf("fetchFunc() error = %v", err)
	}
	defer body.Close()

	if contentType != "image/png" {
		t.Fatalf("contentType = %q, want %q", contentType, "image/png")
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(data) != "s3 image" {
		t.Fatalf("body = %q, want %q", string(data), "s3 image")
	}
}

func TestResolveS3Head(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Fatalf("method = %s, want HEAD", r.Method)
		}
		if r.URL.Path != "/presigned-head" {
			t.Fatalf("path = %q, want /presigned-head", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	originalNewS3Presigner := newS3Presigner
	newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
		return mockPresigner{
			t:            t,
			bucket:       "source-bucket",
			key:          "images/cat.png",
			response:     server.URL + "/presigned-get",
			headResponse: server.URL + "/presigned-head",
		}, nil
	}
	t.Cleanup(func() { newS3Presigner = originalNewS3Presigner })

	req := httptest.NewRequest(http.MethodGet, "http://origin.example/images/cat.png?width=640", nil)
	req.Header.Set("X-Img-Source-Type", "s3")
	req.Header.Set("X-Img-Source-Bucket", "source-bucket")

	_, headFunc, _, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	contentType, err := headFunc()
	if err != nil {
		t.Fatalf("headFunc() error = %v", err)
	}
	if contentType != "image/png" {
		t.Fatalf("contentType = %q, want %q", contentType, "image/png")
	}
}

func TestResolveGatewayFromHeader(t *testing.T) {
	var gotHost string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		if r.URL.RequestURI() != "/images/cat.jpg?width=640" {
			t.Fatalf("RequestURI = %q, want %q", r.URL.RequestURI(), "/images/cat.jpg?width=640")
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("gateway image"))
	}))
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/images/cat.jpg?width=640", nil)
	req.Host = "assets.example"
	req.Header.Set("X-Img-Upstream-Gateway", server.URL)

	sourceURL, headFunc, fetchFunc, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if sourceURL != server.URL+"/images/cat.jpg?width=640" {
		t.Fatalf("sourceURL = %q, want gateway URL", sourceURL)
	}
	if headFunc == nil {
		t.Fatal("headFunc = nil, want function")
	}

	headContentType, err := headFunc()
	if err != nil {
		t.Fatalf("headFunc() error = %v", err)
	}
	if headContentType != "image/jpeg" {
		t.Fatalf("head contentType = %q, want %q", headContentType, "image/jpeg")
	}

	body, contentType, err := fetchFunc()
	if err != nil {
		t.Fatalf("fetchFunc() error = %v", err)
	}
	defer body.Close()

	if gotHost != "assets.example" {
		t.Fatalf("Host = %q, want %q", gotHost, "assets.example")
	}
	if contentType != "image/jpeg" {
		t.Fatalf("contentType = %q, want %q", contentType, "image/jpeg")
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(data) != "gateway image" {
		t.Fatalf("body = %q, want %q", string(data), "gateway image")
	}
}

func TestResolveGatewayStripsTransformQueryParamsFromSource(t *testing.T) {
	var gotRequestURIs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURIs = append(gotRequestURIs, r.URL.RequestURI())
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("gateway image"))
	}))
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/images/cat.jpg?v=123&imwidth=640&foo=bar&f=webp&q=75", nil)
	req.Host = "assets.example"
	req.Header.Set("X-Img-Upstream-Gateway", server.URL)

	sourceURL, headFunc, fetchFunc, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	wantSourceURL := server.URL + "/images/cat.jpg?v=123&foo=bar"
	if sourceURL != wantSourceURL {
		t.Fatalf("sourceURL = %q, want %q", sourceURL, wantSourceURL)
	}

	if _, err := headFunc(); err != nil {
		t.Fatalf("headFunc() error = %v", err)
	}
	body, _, err := fetchFunc()
	if err != nil {
		t.Fatalf("fetchFunc() error = %v", err)
	}
	defer body.Close()

	wantRequestURI := "/images/cat.jpg?v=123&foo=bar"
	if len(gotRequestURIs) != 2 {
		t.Fatalf("got %d upstream requests, want 2", len(gotRequestURIs))
	}
	for _, got := range gotRequestURIs {
		if got != wantRequestURI {
			t.Fatalf("upstream RequestURI = %q, want %q", got, wantRequestURI)
		}
	}
}

func TestResolveGatewayMissingHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://assets.example/images/cat.jpg", nil)

	_, _, _, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for missing X-Img-Upstream-Gateway")
	}
	if !strings.Contains(err.Error(), "X-Img-Upstream-Gateway") {
		t.Fatalf("error = %q, want message mentioning X-Img-Upstream-Gateway", err.Error())
	}
}

func TestResolveGatewayAllowlistAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("allowed image"))
	}))
	defer server.Close()

	serverHost := strings.TrimPrefix(server.URL, "http://")

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", server.URL)

	_, _, _, err := NewResolver(30*time.Second, []string{serverHost}, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil (allowed gateway)", err)
	}
}

func TestResolveGatewayAllowlistAllowsHostOnlyWhenGatewayIncludesPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	serverHostPort := strings.TrimPrefix(server.URL, "http://")
	serverHost := strings.Split(serverHostPort, ":")[0]

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", server.URL)

	_, _, _, err := NewResolver(30*time.Second, []string{serverHost}, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil for host-only allowlist", err)
	}
}

func TestResolveGatewayAllowlistIsCaseInsensitive(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", "http://TRUSTED.EXAMPLE.COM:8080")

	_, _, _, err := NewResolver(30*time.Second, []string{"trusted.example.com"}, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil for case-insensitive host allowlist", err)
	}
}

func TestResolveGatewayAllowlistBlocked(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", "http://evil.example.com")

	_, _, _, err := NewResolver(30*time.Second, []string{"trusted.example.com"}, nil).Resolve(req)
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for blocked gateway")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error = %q, want message mentioning allowlist", err.Error())
	}
}

func TestResolveGatewayAllowlistEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("image"))
	}))
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", server.URL)

	_, _, _, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil (no allowlist)", err)
	}
}

func TestResolveS3MissingBucket(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://origin.example/img/cat.png", nil)
	req.Header.Set("X-Img-Source-Type", "s3")
	// deliberately omit X-Img-Source-Bucket

	_, _, _, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for missing X-Img-Source-Bucket")
	}
	if !strings.Contains(err.Error(), "X-Img-Source-Bucket") {
		t.Fatalf("error = %q, want message mentioning X-Img-Source-Bucket", err.Error())
	}
}

func TestResolveS3BucketAllowlistBlocked(t *testing.T) {
	originalNewS3Presigner := newS3Presigner
	newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
		return mockPresigner{t: t, bucket: "evil-bucket", key: "cat.png", response: "http://ignored"}, nil
	}
	t.Cleanup(func() { newS3Presigner = originalNewS3Presigner })

	req := httptest.NewRequest(http.MethodGet, "http://origin.example/cat.png", nil)
	req.Header.Set("X-Img-Source-Type", "s3")
	req.Header.Set("X-Img-Source-Bucket", "evil-bucket")

	_, _, _, err := NewResolver(30*time.Second, nil, []string{"allowed-bucket"}).Resolve(req)
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for blocked bucket")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error = %q, want message mentioning allowlist", err.Error())
	}
}

func TestResolveS3BucketAllowlistAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("allowed s3 image"))
	}))
	defer server.Close()

	originalNewS3Presigner := newS3Presigner
	newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
		return mockPresigner{t: t, bucket: "allowed-bucket", key: "cat.png", response: server.URL + "/presigned"}, nil
	}
	t.Cleanup(func() { newS3Presigner = originalNewS3Presigner })

	req := httptest.NewRequest(http.MethodGet, "http://origin.example/cat.png", nil)
	req.Header.Set("X-Img-Source-Type", "s3")
	req.Header.Set("X-Img-Source-Bucket", "allowed-bucket")

	_, _, _, err := NewResolver(30*time.Second, nil, []string{"allowed-bucket"}).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil (allowed bucket)", err)
	}
}

func TestFetchHTTPRetryOn5xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok after retry"))
	}))
	defer server.Close()

	d := NewResolver(5*time.Second, nil, nil)
	body, ct, err := d.fetchHTTP(context.Background(), server.URL+"/img.jpg", "")
	if err != nil {
		t.Fatalf("fetchHTTP error = %v, want nil after retry", err)
	}
	defer body.Close()
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	if calls.Load() != 3 {
		t.Errorf("server calls = %d, want 3 (two 500s then success)", calls.Load())
	}
}

func TestFetchHTTPNoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	d := NewResolver(5*time.Second, nil, nil)
	_, _, err := d.fetchHTTP(context.Background(), server.URL+"/img.jpg", "")
	if err == nil {
		t.Fatal("fetchHTTP error = nil, want error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want message mentioning 404", err.Error())
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1 (no retry on 4xx)", calls.Load())
	}
}

func TestNewResolverWithTransport(t *testing.T) {
	d := NewResolverWithTransport(5*time.Second, nil, nil, nil)
	if d.httpClient.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s", d.httpClient.Timeout)
	}
	if d.httpClient.Transport != nil {
		t.Fatalf("expected nil transport (uses default), got %T", d.httpClient.Transport)
	}

	tr := &http.Transport{}
	d2 := NewResolverWithTransport(5*time.Second, nil, nil, tr)
	if d2.httpClient.Transport != tr {
		t.Fatalf("transport not wired through")
	}
}

func TestNewResolverWithEagerPresigner_MockPresigner(t *testing.T) {
	origFactory := newS3Presigner
	defer func() { newS3Presigner = origFactory }()

	mock := mockPresigner{t: t, bucket: "b", key: "k", response: "https://s3.example.com/presigned"}
	newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
		return mock, nil
	}

	r, err := NewResolverWithEagerPresigner(
		context.Background(),
		5*time.Second,
		[]string{"gateway.example.com"},
		[]string{"source-bucket"},
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.presigner != mock {
		t.Fatal("presigner not injected at construction")
	}
}

func TestNewResolverWithEagerPresigner_InitError(t *testing.T) {
	origFactory := newS3Presigner
	defer func() { newS3Presigner = origFactory }()

	newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
		return nil, fmt.Errorf("aws init failed")
	}

	_, err := NewResolverWithEagerPresigner(
		context.Background(),
		5*time.Second,
		nil,
		nil,
		nil,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestResolveGatewaySchemeAdded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("image"))
	}))
	defer server.Close()

	// Extract host:port from server URL (e.g., "http://127.0.0.1:12345" -> "127.0.0.1:12345")
	gatewayHostPort := strings.TrimPrefix(server.URL, "http://")

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/images/cat.jpg", nil)
	// Pass gateway without scheme (just host:port)
	req.Header.Set("X-Img-Upstream-Gateway", gatewayHostPort)

	sourceURL, _, _, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}

	// Verify http:// was prepended
	if !strings.HasPrefix(sourceURL, "http://") {
		t.Fatalf("sourceURL = %q, want to start with http://", sourceURL)
	}
	if !strings.Contains(sourceURL, gatewayHostPort) {
		t.Fatalf("sourceURL = %q, want to contain %q", sourceURL, gatewayHostPort)
	}
}

func TestResolveGatewayContextCancellation(t *testing.T) {
	// Create a server that never responds (blocks forever)
	blockingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never write response, just block
		select {}
	}))
	defer blockingServer.Close()

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/images/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", blockingServer.URL)

	// Create a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)

	sourceURL, _, fetchFunc, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if sourceURL == "" {
		t.Fatal("sourceURL is empty")
	}

	// Cancel context before calling fetchFunc
	cancel()

	// fetchFunc should respect the cancelled context and return a context error
	_, _, err = fetchFunc()
	if err == nil {
		t.Fatal("fetchFunc() error = nil, want context error after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchFunc() error = %v, want context.Canceled", err)
	}
}

func TestResolveS3PresignError(t *testing.T) {
	origFactory := newS3Presigner
	defer func() { newS3Presigner = origFactory }()

	// Mock presigner that returns an error on PresignGetObject
	errorPresigner := &mockPresignerWithError{
		t:      t,
		bucket: "source-bucket",
		key:    "images/cat.png",
		err:    fmt.Errorf("presign failed: access denied"),
	}
	newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
		return errorPresigner, nil
	}

	req := httptest.NewRequest(http.MethodGet, "http://origin.example/images/cat.png", nil)
	req.Header.Set("X-Img-Source-Type", "s3")
	req.Header.Set("X-Img-Source-Bucket", "source-bucket")

	_, _, _, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err == nil {
		t.Fatal("Resolve() error = nil, want error from presigner")
	}
	if !strings.Contains(err.Error(), "presign failed") {
		t.Fatalf("error = %q, want message mentioning presign failure", err.Error())
	}
}

func TestResolveRequestURIEmptyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Fatalf("path = %q, want /", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("root image"))
	}))
	defer server.Close()

	// Create request with just "/" path (no additional path segments)
	req := httptest.NewRequest(http.MethodGet, "http://assets.example/", nil)
	req.Header.Set("X-Img-Upstream-Gateway", server.URL)

	sourceURL, _, fetchFunc, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}

	// Verify sourceURL is valid and includes the path
	if sourceURL == "" {
		t.Fatal("sourceURL is empty")
	}
	if !strings.HasSuffix(sourceURL, "/") {
		t.Fatalf("sourceURL = %q, want to end with /", sourceURL)
	}

	// Verify fetchFunc works without panic
	body, ct, err := fetchFunc()
	if err != nil {
		t.Fatalf("fetchFunc() error = %v, want nil", err)
	}
	defer body.Close()

	if ct != "image/jpeg" {
		t.Fatalf("contentType = %q, want image/jpeg", ct)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(data) != "root image" {
		t.Fatalf("body = %q, want %q", string(data), "root image")
	}
}

func TestResolveGatewayFTPSchemeRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", "ftp://example.com")

	_, _, _, err := NewResolver(30*time.Second, []string{"example.com"}, nil).Resolve(req)
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for ftp scheme")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %q, want message containing \"not allowed\"", err.Error())
	}
}

func TestResolveGatewayFileSchemeRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", "file:///etc/passwd")

	_, _, _, err := NewResolver(30*time.Second, nil, nil).Resolve(req)
	if err == nil {
		t.Fatal("Resolve() error = nil, want error for file scheme")
	}
	if !strings.Contains(err.Error(), "not allowed") && !strings.Contains(err.Error(), "invalid value") {
		t.Fatalf("error = %q, want rejection error", err.Error())
	}
}

func TestResolveGatewayHTTPSAllowed(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("https image"))
	}))
	defer server.Close()

	serverHost := strings.TrimPrefix(server.URL, "https://")

	req := httptest.NewRequest(http.MethodGet, "http://assets.example/img/cat.jpg", nil)
	req.Header.Set("X-Img-Upstream-Gateway", server.URL)

	// Allowlist must include the host (with port) for https
	_, _, _, err := NewResolver(30*time.Second, []string{serverHost}, nil).Resolve(req)
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil for https gateway", err)
	}
}

// mockPresignerWithError is a mock presigner that returns an error
type mockPresignerWithError struct {
	t      *testing.T
	bucket string
	key    string
	err    error
}

func (m *mockPresignerWithError) PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	m.t.Helper()
	if got := aws.ToString(params.Bucket); got != m.bucket {
		m.t.Fatalf("Bucket = %q, want %q", got, m.bucket)
	}
	if got := aws.ToString(params.Key); got != m.key {
		m.t.Fatalf("Key = %q, want %q", got, m.key)
	}
	return nil, m.err
}

func (m *mockPresignerWithError) PresignHeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	m.t.Helper()
	return nil, m.err
}
