package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/metrics"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/requestid"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Resolver determines the upstream source for an image.
type Resolver interface {
	Resolve(r *http.Request) (sourceURL string, headFunc func() (string, error), fetchFunc func() (io.ReadCloser, string, error), err error)
}

type s3Presigner interface {
	PresignGetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignHeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

var newS3Presigner = func(ctx context.Context) (s3Presigner, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s3.NewPresignClient(s3.NewFromConfig(cfg)), nil
}

// StatusError represents an HTTP error status returned by the upstream source.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.Code)
}

// Retryable returns true if the status code represents a temporary server error (5xx).
func (e *StatusError) Retryable() bool {
	return e.Code >= 500
}

// DefaultResolver resolves image sources from either S3 or the upstream gateway.
type DefaultResolver struct {
	httpClient           *http.Client
	allowedGateways      []string
	allowedSourceBuckets []string
	presignerOnce        sync.Once
	presigner            s3Presigner
	presignerErr         error
}

// NewResolver creates the default upstream resolver with the given HTTP timeout.
func NewResolver(timeout time.Duration, allowedGateways []string, allowedSourceBuckets []string) *DefaultResolver {
	return &DefaultResolver{
		httpClient:           &http.Client{Timeout: timeout},
		allowedGateways:      allowedGateways,
		allowedSourceBuckets: allowedSourceBuckets,
	}
}

// NewResolverWithTransport creates a DefaultResolver using the provided
// transport. If transport is nil, the default http.Transport is used.
func NewResolverWithTransport(timeout time.Duration, allowedGateways []string, allowedSourceBuckets []string, transport http.RoundTripper) *DefaultResolver {
	return &DefaultResolver{
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		allowedGateways:      allowedGateways,
		allowedSourceBuckets: allowedSourceBuckets,
	}
}

// NewResolverWithEagerPresigner creates a DefaultResolver that initializes the
// S3 presigner at construction time. Returns an error if presigner init fails,
// enabling fail-fast at startup. transport may be nil (uses default).
func NewResolverWithEagerPresigner(ctx context.Context, timeout time.Duration, allowedGateways []string, allowedSourceBuckets []string, transport http.RoundTripper) (*DefaultResolver, error) {
	p, err := newS3Presigner(ctx)
	if err != nil {
		return nil, fmt.Errorf("upstream: init S3 presigner: %w", err)
	}
	return &DefaultResolver{
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		allowedGateways:      allowedGateways,
		allowedSourceBuckets: allowedSourceBuckets,
		presigner:            p,
	}, nil
}

// Resolve returns the source URL that imgproxy should read plus HEAD and fallback fetch functions.
func (d *DefaultResolver) Resolve(r *http.Request) (string, func() (string, error), func() (io.ReadCloser, string, error), error) {
	if r.Header.Get("X-Img-Source-Type") == "s3" {
		return d.resolveS3(r)
	}

	rawGateway := strings.TrimSpace(r.Header.Get("X-Img-Upstream-Gateway"))
	if rawGateway == "" {
		return "", nil, nil, fmt.Errorf("X-Img-Upstream-Gateway header is required")
	}

	if !strings.Contains(rawGateway, "://") {
		rawGateway = "http://" + rawGateway
	}
	gatewayURL, err := url.Parse(rawGateway)
	if err != nil || gatewayURL.Host == "" {
		return "", nil, nil, fmt.Errorf("X-Img-Upstream-Gateway has invalid value")
	}

	if gatewayURL.Scheme != "http" && gatewayURL.Scheme != "https" {
		return "", nil, nil, fmt.Errorf("upstream gateway scheme %q not allowed: only http/https permitted", gatewayURL.Scheme)
	}

	if len(d.allowedGateways) > 0 {
		allowed := false
		for _, g := range d.allowedGateways {
			if gatewayURL.Host == g {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", nil, nil, fmt.Errorf("upstream gateway not in allowlist")
		}
	}

	sourceURL := gatewayURL.Scheme + "://" + gatewayURL.Host + requestURI(r)
	headFunc := func() (string, error) {
		return d.headHTTP(r.Context(), sourceURL, r.Host)
	}
	fetchFunc := func() (io.ReadCloser, string, error) {
		return d.fetchHTTP(r.Context(), sourceURL, r.Host)
	}

	return sourceURL, headFunc, fetchFunc, nil
}

func (d *DefaultResolver) resolveS3(r *http.Request) (string, func() (string, error), func() (io.ReadCloser, string, error), error) {
	bucket := strings.TrimSpace(r.Header.Get("X-Img-Source-Bucket"))
	if bucket == "" {
		return "", nil, nil, fmt.Errorf("X-Img-Source-Bucket is required for s3 source")
	}

	if len(d.allowedSourceBuckets) > 0 {
		allowed := false
		for _, b := range d.allowedSourceBuckets {
			if bucket == b {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", nil, nil, fmt.Errorf("source bucket %q not in allowlist", bucket)
		}
	}

	d.presignerOnce.Do(func() {
		if d.presigner == nil {
			d.presigner, d.presignerErr = newS3Presigner(context.Background())
		}
	})
	if d.presignerErr != nil {
		return "", nil, nil, d.presignerErr
	}

	start := time.Now()
	ctx, span := tracing.Tracer().Start(r.Context(), "upstream.presign",
		trace.WithAttributes(attribute.String("upstream.type", "s3")),
	)
	defer span.End()
	presigned, err := d.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(strings.TrimPrefix(r.URL.Path, "/")),
	})
	if err != nil {
		metrics.ObserveUpstreamFetch("error", time.Since(start).Seconds())
		span.RecordError(err)
		return "", nil, nil, err
	}

	headPresigned, err := d.presigner.PresignHeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(strings.TrimPrefix(r.URL.Path, "/")),
	})
	if err != nil {
		metrics.ObserveUpstreamFetch("error", time.Since(start).Seconds())
		span.RecordError(err)
		return "", nil, nil, err
	}
	metrics.ObserveUpstreamFetch("success", time.Since(start).Seconds())

	sourceURL := presigned.URL
	fetchFunc := func() (io.ReadCloser, string, error) {
		return d.fetchHTTP(r.Context(), sourceURL, "")
	}

	headURL := headPresigned.URL
	headFunc := func() (string, error) {
		return d.headHTTP(r.Context(), headURL, "")
	}

	return sourceURL, headFunc, fetchFunc, nil
}

func requestURI(r *http.Request) string {
	uri := r.URL.RequestURI()
	if uri == "" {
		return "/"
	}
	return uri
}

func (d *DefaultResolver) fetchHTTP(ctx context.Context, rawURL, host string) (io.ReadCloser, string, error) {
	const maxAttempts = 3
	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := time.Duration(100<<(attempt-1)) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(delay):
			}
		}

		rc, ct, err := d.doFetch(ctx, rawURL, host)
		if err == nil {
			return rc, ct, nil
		}
		lastErr = err

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, "", err
		}
		var statusErr *StatusError
		if errors.As(err, &statusErr) && !statusErr.Retryable() {
			return nil, "", err
		}
	}
	return nil, "", lastErr
}

// headHTTP sends an HTTP HEAD request and returns the Content-Type header.
// On 405 Method Not Allowed or any error, returns ("", nil) so the caller
// can fall through to attempting a transform.
func (d *DefaultResolver) headHTTP(ctx context.Context, rawURL, host string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return "", nil
	}
	if host != "" {
		req.Host = host
	}
	if id := requestid.FromContext(ctx); id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	res, err := d.httpClient.Do(req)
	if err != nil {
		return "", nil
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusMethodNotAllowed {
		return "", nil
	}
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return "", nil
	}
	return res.Header.Get("Content-Type"), nil
}

func (d *DefaultResolver) doFetch(ctx context.Context, rawURL, host string) (io.ReadCloser, string, error) {
	ctx, span := tracing.Tracer().Start(ctx, "upstream.fetch",
		trace.WithAttributes(attribute.String("upstream.type", "http")),
	)
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		span.RecordError(err)
		return nil, "", err
	}
	if host != "" {
		req.Host = host
	}
	if id := requestid.FromContext(ctx); id != "" {
		req.Header.Set("X-Request-Id", id)
	}

	res, err := d.httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		return nil, "", err
	}
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		res.Body.Close()
		err := &StatusError{Code: res.StatusCode}
		span.SetStatus(codes.Error, fmt.Sprintf("unexpected status %d", res.StatusCode))
		span.RecordError(err)
		return nil, "", err
	}

	return res.Body, res.Header.Get("Content-Type"), nil
}
