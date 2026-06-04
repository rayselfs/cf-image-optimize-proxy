package config

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("IMGPROXY_URL", "")
	t.Setenv("CACHE_S3_BUCKET", "source-images")
	t.Setenv("CACHE_S3_REGION", "")
	t.Setenv("MAX_WIDTH", "")
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ListenAddr != ":9999" {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, ":9999")
	}
	if cfg.ImgproxyURL != "http://localhost:8081" {
		t.Fatalf("ImgproxyURL = %q, want %q", cfg.ImgproxyURL, "http://localhost:8081")
	}
	if cfg.CacheS3Bucket != "source-images" {
		t.Fatalf("CacheS3Bucket = %q, want %q", cfg.CacheS3Bucket, "source-images")
	}
	if cfg.CacheS3Region != "us-west-2" {
		t.Fatalf("CacheS3Region = %q, want %q", cfg.CacheS3Region, "us-west-2")
	}
	if cfg.MaxWidth != 1920 {
		t.Fatalf("MaxWidth = %d, want %d", cfg.MaxWidth, 1920)
	}
}

func TestRequiredCacheS3Bucket(t *testing.T) {
	t.Setenv("CACHE_S3_BUCKET", "")
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want error")
	}
}

func TestCustomConfig(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("IMGPROXY_URL", "http://imgproxy:8080")
	t.Setenv("CACHE_S3_BUCKET", "custom-bucket")
	t.Setenv("CACHE_S3_REGION", "ap-northeast-1")
	t.Setenv("MAX_WIDTH", "2048")
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ListenAddr != ":9090" {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, ":9090")
	}
	if cfg.ImgproxyURL != "http://imgproxy:8080" {
		t.Fatalf("ImgproxyURL = %q, want %q", cfg.ImgproxyURL, "http://imgproxy:8080")
	}
	if cfg.CacheS3Bucket != "custom-bucket" {
		t.Fatalf("CacheS3Bucket = %q, want %q", cfg.CacheS3Bucket, "custom-bucket")
	}
	if cfg.CacheS3Region != "ap-northeast-1" {
		t.Fatalf("CacheS3Region = %q, want %q", cfg.CacheS3Region, "ap-northeast-1")
	}
	if cfg.MaxWidth != 2048 {
		t.Fatalf("MaxWidth = %d, want %d", cfg.MaxWidth, 2048)
	}
}

func TestLoadCSV_Empty(t *testing.T) {
	t.Setenv("CF_ORIGIN_SECRET", "")
	t.Setenv("CACHE_S3_BUCKET", "test-bucket")
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.OriginSecrets) != 0 {
		t.Fatalf("OriginSecrets = %v, want empty", cfg.OriginSecrets)
	}
}

func TestLoadCSV_Single(t *testing.T) {
	t.Setenv("CF_ORIGIN_SECRET", "mysecret")
	t.Setenv("CACHE_S3_BUCKET", "test-bucket")
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.OriginSecrets) != 1 || cfg.OriginSecrets[0] != "mysecret" {
		t.Fatalf("OriginSecrets = %v, want [mysecret]", cfg.OriginSecrets)
	}
}

func TestLoadCSV_Multiple(t *testing.T) {
	t.Setenv("CF_ORIGIN_SECRET", "new-secret,old-secret")
	t.Setenv("CACHE_S3_BUCKET", "test-bucket")
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.OriginSecrets) != 2 {
		t.Fatalf("OriginSecrets len = %d, want 2", len(cfg.OriginSecrets))
	}
	if cfg.OriginSecrets[0] != "new-secret" || cfg.OriginSecrets[1] != "old-secret" {
		t.Fatalf("OriginSecrets = %v, want [new-secret old-secret]", cfg.OriginSecrets)
	}
}

func TestLoadCSV_AllowedGateways(t *testing.T) {
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "api.example.com,cdn.example.com")
	t.Setenv("CACHE_S3_BUCKET", "test-bucket")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.AllowedUpstreamGateways) != 2 {
		t.Fatalf("AllowedUpstreamGateways len = %d, want 2", len(cfg.AllowedUpstreamGateways))
	}
	if cfg.AllowedUpstreamGateways[0] != "api.example.com" || cfg.AllowedUpstreamGateways[1] != "cdn.example.com" {
		t.Fatalf("AllowedUpstreamGateways = %v, want [api.example.com cdn.example.com]", cfg.AllowedUpstreamGateways)
	}
}

func TestLoadCSV_TrimsWhitespace(t *testing.T) {
	t.Setenv("CF_ORIGIN_SECRET", " secret-a , secret-b ")
	t.Setenv("CACHE_S3_BUCKET", "test-bucket")
	t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
	t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.OriginSecrets) != 2 {
		t.Fatalf("OriginSecrets len = %d, want 2", len(cfg.OriginSecrets))
	}
	if cfg.OriginSecrets[0] != "secret-a" || cfg.OriginSecrets[1] != "secret-b" {
		t.Fatalf("OriginSecrets = %v, want trimmed values", cfg.OriginSecrets)
	}
}

func TestAllowlistValidation(t *testing.T) {
	tests := []struct {
		name                    string
		allowedUpstreamGateways string
		allowedSourceBuckets    string
		wantErr                 bool
	}{
		{
			name:                    "both allowlists set",
			allowedUpstreamGateways: "api.example.com",
			allowedSourceBuckets:    "my-bucket",
			wantErr:                 false,
		},
		{
			name:                 "gateways empty",
			allowedSourceBuckets: "my-bucket",
			wantErr:              true,
		},
		{
			name:                    "buckets empty",
			allowedUpstreamGateways: "api.example.com",
			wantErr:                 true,
		},
		{
			name:    "both empty",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_S3_BUCKET", "test-bucket")
			t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", tc.allowedUpstreamGateways)
			t.Setenv("ALLOWED_SOURCE_BUCKETS", tc.allowedSourceBuckets)

			_, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}


func TestLoadInvalidSettings(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
		value  string
	}{
		// MAX_WIDTH invalid values
		{"MAX_WIDTH=0", "MAX_WIDTH", "0"},
		{"MAX_WIDTH=-1", "MAX_WIDTH", "-1"},
		{"MAX_WIDTH=abc", "MAX_WIDTH", "abc"},
		// UPSTREAM_TIMEOUT invalid values
		{"UPSTREAM_TIMEOUT=0", "UPSTREAM_TIMEOUT", "0"},
		{"UPSTREAM_TIMEOUT=-1", "UPSTREAM_TIMEOUT", "-1"},
		{"UPSTREAM_TIMEOUT=abc", "UPSTREAM_TIMEOUT", "abc"},
		// IMGPROXY_TIMEOUT invalid values
		{"IMGPROXY_TIMEOUT=0", "IMGPROXY_TIMEOUT", "0"},
		// SHUTDOWN_TIMEOUT invalid values
		{"SHUTDOWN_TIMEOUT=0", "SHUTDOWN_TIMEOUT", "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_S3_BUCKET", "test")
			t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
			t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")
			t.Setenv(tc.envVar, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want error for %s=%s", tc.envVar, tc.value)
			}
		})
	}
}

func TestLoadValidSettings(t *testing.T) {
	tests := []struct {
		name                string
		maxWidth            string
		upstreamTimeout     string
		wantMaxWidth        int
		wantUpstreamTimeout time.Duration
	}{
		{
			name:                "custom valid values",
			maxWidth:            "640",
			upstreamTimeout:     "10",
			wantMaxWidth:        640,
			wantUpstreamTimeout: 10 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_S3_BUCKET", "test")
			t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
			t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")
			t.Setenv("MAX_WIDTH", tc.maxWidth)
			t.Setenv("UPSTREAM_TIMEOUT", tc.upstreamTimeout)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.MaxWidth != tc.wantMaxWidth {
				t.Fatalf("MaxWidth = %d, want %d", cfg.MaxWidth, tc.wantMaxWidth)
			}
			if cfg.UpstreamTimeout != tc.wantUpstreamTimeout {
				t.Fatalf("UpstreamTimeout = %v, want %v", cfg.UpstreamTimeout, tc.wantUpstreamTimeout)
			}
		})
	}
}

func TestInvalidImgproxyURL(t *testing.T) {
	tests := []struct {
		name        string
		imgproxyURL string
		shouldFail  bool
	}{
		{"valid http", "http://localhost:8081", false},
		{"valid https", "https://imgproxy.example.com", false},
		{"invalid ftp scheme", "ftp://host", true},
		{"invalid no scheme", "localhost:8081", true},
		{"invalid malformed", "not-a-url", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_S3_BUCKET", "test-bucket")
			t.Setenv("ALLOWED_UPSTREAM_GATEWAYS", "upstream.com")
			t.Setenv("ALLOWED_SOURCE_BUCKETS", "source-bucket")
			t.Setenv("IMGPROXY_URL", tc.imgproxyURL)

			_, err := Load()
			if tc.shouldFail && err == nil {
				t.Fatalf("Load() error = nil, want error for IMGPROXY_URL=%q", tc.imgproxyURL)
			}
			if !tc.shouldFail && err != nil {
				t.Fatalf("Load() error = %v, want nil for IMGPROXY_URL=%q", err, tc.imgproxyURL)
			}
		})
	}
}
