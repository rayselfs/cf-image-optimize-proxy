package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/cache"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/coalesce"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/imgproxy"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/upstream"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type integrationMockS3 struct {
	mu      sync.Mutex
	objects map[string]integrationS3Object
}

type integrationS3Object struct {
	body        []byte
	contentType string
}

func newIntegrationMockS3() *integrationMockS3 {
	return &integrationMockS3{objects: make(map[string]integrationS3Object)}
}

func (m *integrationMockS3) GetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	object, ok := m.objects[aws.ToString(params.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}

	return &s3.GetObjectOutput{
		Body:        io.NopCloser(bytes.NewReader(object.body)),
		ContentType: aws.String(object.contentType),
	}, nil
}

func (m *integrationMockS3) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, err := io.ReadAll(params.Body)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[aws.ToString(params.Key)] = integrationS3Object{
		body:        body,
		contentType: aws.ToString(params.ContentType),
	}

	return &s3.PutObjectOutput{}, nil
}

func (m *integrationMockS3) HeadBucket(_ context.Context, _ *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, nil
}

func TestIntegrationImageOptimizationFlow(t *testing.T) {
	const (
		originalImage    = "fake-png-image"
		transformedImage = "fake-webp-image"
	)

	var upstreamCalls int
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.Host != "assets.example.com" {
			t.Fatalf("upstream host = %q, want assets.example.com", r.Host)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(originalImage))
	}))
	defer upstreamServer.Close()

	var imgproxyCalls int
	imgproxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imgproxyCalls++
		if !strings.Contains(r.URL.Path, "/unsafe/rs:fit:640/q:75/webp/plain/") {
			t.Fatalf("imgproxy path = %q, want resize/quality/format path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write([]byte(transformedImage))
	}))
	defer imgproxyServer.Close()

	s3Cache := cache.NewS3Cache(newIntegrationMockS3(), "test-bucket")
	h := New(
		s3Cache,
		imgproxy.NewClient(imgproxyServer.URL, 30*time.Second),
		upstream.NewResolver(30*time.Second, nil, nil),
		coalesce.New(),
		1920,
		75,
	)

	assertRequest(t, h, "https://assets.example.com/images/hero.png?imwidth=640&f=webp&q=75", upstreamServer.URL, transformedImage, "image/webp", "MISS")
	assertRequest(t, h, "https://assets.example.com/images/hero.png?imwidth=640&f=webp&q=75", upstreamServer.URL, transformedImage, "image/webp", "HIT")
	assertRequest(t, h, "https://assets.example.com/images/hero.png", upstreamServer.URL, originalImage, "image/png", "")

	if upstreamCalls != 2 {
		t.Fatalf("upstream calls = %d, want 2", upstreamCalls)
	}
	if imgproxyCalls != 1 {
		t.Fatalf("imgproxy calls = %d, want 1", imgproxyCalls)
	}
}

func assertRequest(t *testing.T, h http.Handler, target, gatewayURL, wantBody, wantContentType, wantXCache string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-Img-Upstream-Gateway", gatewayURL)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != wantBody {
		t.Fatalf("body = %q, want %q", got, wantBody)
	}
	if got := w.Header().Get("Content-Type"); got != wantContentType {
		t.Fatalf("Content-Type = %q, want %q", got, wantContentType)
	}
	if got := w.Header().Get("X-Cache"); got != wantXCache {
		t.Fatalf("X-Cache = %q, want %q", got, wantXCache)
	}
	if wantXCache != "" {
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000" {
			t.Fatalf("Cache-Control = %q, want public, max-age=31536000", got)
		}
	}
}

func TestIntegrationImgproxyFallback(t *testing.T) {
	const (
		originalImage = "original-image-bytes"
	)

	var imgproxyCalls int
	imgproxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imgproxyCalls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("imgproxy error"))
	}))
	defer imgproxyServer.Close()

	var upstreamCalls int
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte(originalImage))
	}))
	defer upstreamServer.Close()

	mockCache := newIntegrationMockS3()
	s3Cache := cache.NewS3Cache(mockCache, "test-bucket")
	h := New(
		s3Cache,
		imgproxy.NewClient(imgproxyServer.URL, 30*time.Second),
		upstream.NewResolver(30*time.Second, nil, nil),
		coalesce.New(),
		1920,
		75,
	)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/image.jpg?imwidth=640&f=webp&q=75", nil)
	req.Header.Set("X-Img-Upstream-Gateway", upstreamServer.URL)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != originalImage {
		t.Fatalf("body = %q, want %q", got, originalImage)
	}
	if got := w.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", got)
	}
	if imgproxyCalls != 1 {
		t.Fatalf("imgproxy calls = %d, want 1 (attempted transform)", imgproxyCalls)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (HEAD + fallback fetch)", upstreamCalls)
	}

	mockCache.mu.Lock()
	cacheSize := len(mockCache.objects)
	mockCache.mu.Unlock()
	if cacheSize > 0 {
		t.Fatalf("cache should not store fallback response, but found %d objects", cacheSize)
	}
}
