package cache

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type mockS3 struct {
	getOutput     *s3.GetObjectOutput
	getErr        error
	putInput      *s3.PutObjectInput
	putErr        error
	headBucketErr error
}

func (m *mockS3) GetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return m.getOutput, m.getErr
}

func (m *mockS3) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.putInput = params
	return &s3.PutObjectOutput{}, m.putErr
}

func (m *mockS3) HeadBucket(_ context.Context, _ *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, m.headBucketErr
}

func TestKeyFromRequest(t *testing.T) {
	tests := []struct {
		host   string
		path   string
		params ImageParams
		want   string
	}{
		// Existing cases
		{"stream.viverse.com", "/assets/hero", ImageParams{640, "webp", 75}, "stream.viverse.com/assets/hero/640_webp_75"},
		{"stream.viverse.com", "assets/hero", ImageParams{640, "webp", 75}, "stream.viverse.com/assets/hero/640_webp_75"},
		{"example.com", "/img/logo.png", ImageParams{320, "jpeg", 90}, "example.com/img/logo.png/320_jpeg_90"},

		// Edge cases: document cache key format invariant {host}/{path}/{width}_{format}_{quality}
		// Root path: leading / is trimmed, resulting in empty path segment
		{"example.com", "/", ImageParams{640, "webp", 75}, "example.com//640_webp_75"},
		// Host with port: port is included in the key (part of host)
		{"example.com:8080", "/images/photo.jpg", ImageParams{960, "avif", 80}, "example.com:8080/images/photo.jpg/960_avif_80"},
		// Percent-encoded path: preserved as-is (not decoded by KeyFromRequest)
		{"cdn.example.com", "/images%2Ffoo.jpg", ImageParams{320, "jpeg", 85}, "cdn.example.com/images%2Ffoo.jpg/320_jpeg_85"},
		// Path with spaces: preserved as-is in the key
		{"example.com", "/my image.jpg", ImageParams{640, "webp", 75}, "example.com/my image.jpg/640_webp_75"},
		// Normal case with all params: demonstrates full format
		{"example.com", "/assets/banner.png", ImageParams{1280, "webp", 90}, "example.com/assets/banner.png/1280_webp_90"},
	}
	for _, tt := range tests {
		got := KeyFromRequest(tt.host, tt.path, tt.params)
		if got != tt.want {
			t.Errorf("KeyFromRequest(%q, %q, %+v) = %q, want %q", tt.host, tt.path, tt.params, got, tt.want)
		}
	}
}

func TestCacheHit(t *testing.T) {
	body := io.NopCloser(strings.NewReader("image-data"))
	mock := &mockS3{
		getOutput: &s3.GetObjectOutput{
			Body:        body,
			ContentType: aws.String("image/webp"),
		},
	}
	c := NewS3Cache(mock, "test-bucket")
	rc, ct, err := c.Get(context.Background(), "some/key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ct != "image/webp" {
		t.Errorf("contentType = %q, want %q", ct, "image/webp")
	}
	rc.Close()
}

func TestCacheMiss(t *testing.T) {
	mock := &mockS3{getErr: &types.NoSuchKey{}}
	c := NewS3Cache(mock, "test-bucket")
	_, _, err := c.Get(context.Background(), "missing/key")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCachePut(t *testing.T) {
	mock := &mockS3{}
	c := NewS3Cache(mock, "test-bucket")
	body := strings.NewReader("image-data")
	err := c.Put(context.Background(), "some/key", body, "image/webp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.putInput == nil {
		t.Fatal("PutObject was not called")
	}
	if aws.ToString(mock.putInput.Key) != "some/key" {
		t.Errorf("key = %q, want %q", aws.ToString(mock.putInput.Key), "some/key")
	}
	if aws.ToString(mock.putInput.ContentType) != "image/webp" {
		t.Errorf("contentType = %q, want %q", aws.ToString(mock.putInput.ContentType), "image/webp")
	}
	if aws.ToString(mock.putInput.CacheControl) != "public, max-age=31536000" {
		t.Errorf("cacheControl = %q, want %q", aws.ToString(mock.putInput.CacheControl), "public, max-age=31536000")
	}
}

func TestS3CachePutInvalidContentType(t *testing.T) {
	mock := &mockS3{}
	c := NewS3Cache(mock, "test-bucket")
	err := c.Put(context.Background(), "some/key", strings.NewReader("data"), "image/png\r\nX-Bad: 1")
	if err == nil {
		t.Fatal("expected error for CRLF content type, got nil")
	}
	if mock.putInput != nil {
		t.Fatal("PutObject should not be called when content type is invalid")
	}
}

type mockS3Client struct {
	putCalls int
	putErr   error
	getErr   error
}

func (m *mockS3Client) GetObject(_ context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, m.getErr
}

func (m *mockS3Client) PutObject(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.putCalls++
	return &s3.PutObjectOutput{}, m.putErr
}

func (m *mockS3Client) HeadBucket(_ context.Context, _ *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, nil
}

type mockS3Uploader struct {
	calls     int
	err       error
	lastInput *s3.PutObjectInput
}

func (m *mockS3Uploader) Upload(_ context.Context, input *s3.PutObjectInput, _ ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
	m.calls++
	m.lastInput = input
	return &manager.UploadOutput{}, m.err
}

func TestS3CachePutStreaming(t *testing.T) {
	mockUploader := &mockS3Uploader{}
	c := NewS3CacheWithMultipart(&mockS3Client{}, "bucket", mockUploader, 1)

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("streamed-data"))
		pw.Close()
	}()

	if err := c.Put(context.Background(), "key/stream", pr, "image/webp"); err != nil {
		t.Fatalf("Put streaming error: %v", err)
	}
	if mockUploader.calls != 1 {
		t.Fatalf("uploader.Upload calls = %d, want 1 (non-seekable reader should use multipart)", mockUploader.calls)
	}
	if mockUploader.lastInput == nil || aws.ToString(mockUploader.lastInput.Key) != "key/stream" {
		t.Errorf("unexpected upload input: %+v", mockUploader.lastInput)
	}
}

