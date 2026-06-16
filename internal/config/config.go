package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr      = ":9999"
	defaultCacheS3Region   = "us-west-2"
	defaultMaxWidth        = 1920
	defaultUpstreamTimeout = 30 * time.Second
	defaultImgproxyTimeout = 30 * time.Second
	defaultShutdownTimeout = 25 * time.Second
	defaultQuality         = 75
)

// Config holds the service configuration loaded from environment variables.
type Config struct {
	ListenAddr              string
	ImgproxyURL             string
	CacheS3Bucket           string
	CacheS3Region           string
	MaxWidth                int
	UpstreamTimeout         time.Duration
	ImgproxyTimeout         time.Duration
	ShutdownTimeout         time.Duration
	OriginSecrets           []string
	AllowedUpstreamGateways []string
	AllowedSourceBuckets    []string
	DefaultQuality          int
}

// Load reads service configuration from environment variables.
func Load() (*Config, error) {
	maxWidth, err := loadPositiveInt("MAX_WIDTH", defaultMaxWidth)
	if err != nil {
		return nil, err
	}

	upstreamTimeout, err := loadDurationSeconds("UPSTREAM_TIMEOUT", defaultUpstreamTimeout)
	if err != nil {
		return nil, err
	}

	imgproxyTimeout, err := loadDurationSeconds("IMGPROXY_TIMEOUT", defaultImgproxyTimeout)
	if err != nil {
		return nil, err
	}

	shutdownTimeout, err := loadDurationSeconds("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return nil, err
	}

	defaultQualityVal, err := loadPositiveInt("DEFAULT_QUALITY", defaultQuality)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		ListenAddr:              envOrDefault("LISTEN_ADDR", defaultListenAddr),
		ImgproxyURL:             strings.TrimSpace(os.Getenv("IMGPROXY_URL")),
		CacheS3Bucket:           strings.TrimSpace(os.Getenv("CACHE_S3_BUCKET")),
		CacheS3Region:           envOrDefault("CACHE_S3_REGION", defaultCacheS3Region),
		MaxWidth:                maxWidth,
		UpstreamTimeout:         upstreamTimeout,
		ImgproxyTimeout:         imgproxyTimeout,
		ShutdownTimeout:         shutdownTimeout,
		OriginSecrets:           loadCSV("CF_ORIGIN_SECRET"),
		AllowedUpstreamGateways: loadCSV("ALLOWED_UPSTREAM_GATEWAYS"),
		AllowedSourceBuckets:    loadCSV("ALLOWED_SOURCE_BUCKETS"),
		DefaultQuality:          defaultQualityVal,
	}

	if cfg.CacheS3Bucket == "" {
		return nil, fmt.Errorf("CACHE_S3_BUCKET is required")
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func loadPositiveInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}

	return n, nil
}

func loadDurationSeconds(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}

	secs, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer (seconds): %w", name, err)
	}
	if secs <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}

	return time.Duration(secs) * time.Second, nil
}

func loadCSV(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (c *Config) Validate() error {
	if len(c.AllowedUpstreamGateways) == 0 {
		return fmt.Errorf("ALLOWED_UPSTREAM_GATEWAYS is empty; configuration is required")
	}
	if len(c.AllowedSourceBuckets) == 0 {
		return fmt.Errorf("ALLOWED_SOURCE_BUCKETS is empty; configuration is required")
	}

	if c.ImgproxyURL == "" {
		return fmt.Errorf("IMGPROXY_URL is required")
	}
	imgproxyURL, err := url.Parse(c.ImgproxyURL)
	if err != nil || imgproxyURL.Host == "" || (imgproxyURL.Scheme != "http" && imgproxyURL.Scheme != "https") {
		return fmt.Errorf("IMGPROXY_URL %q is not a valid HTTP URL", c.ImgproxyURL)
	}

	if c.DefaultQuality < 1 || c.DefaultQuality > 100 {
		return fmt.Errorf("DEFAULT_QUALITY must be between 1 and 100")
	}

	return nil
}
