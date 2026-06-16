package imgproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/metrics"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/requestid"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TransformParams holds parameters for image transformation.
type TransformParams struct {
	Width   int
	Format  string
	Quality int
}

// Transformer is the interface for image transformation.
type Transformer interface {
	Transform(ctx context.Context, sourceURL string, params TransformParams) (io.ReadCloser, string, error)
}

// Client is an HTTP client for imgproxy.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new imgproxy Client with the given base URL and HTTP timeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// NewClientWithTransport creates a new imgproxy Client using the provided
// transport. If transport is nil, the default http.Transport is used.
// The per-request timeout is enforced by the http.Client.Timeout field.
func NewClientWithTransport(baseURL string, timeout time.Duration, transport http.RoundTripper) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}
}

// buildProcessingURL constructs the imgproxy processing URL.
func buildProcessingURL(baseURL, sourceURL string, params TransformParams) string {
	format := params.Format
	if format == "jpeg" {
		format = "jpg"
	}
	return fmt.Sprintf("%s/unsafe/rs:fit:%d/q:%d/%s/plain/%s", baseURL, params.Width, params.Quality, format, url.PathEscape(sourceURL))
}

// Transform fetches the transformed image from imgproxy.
func (c *Client) Transform(ctx context.Context, sourceURL string, params TransformParams) (io.ReadCloser, string, error) {
	ctx, span := tracing.Tracer().Start(ctx, "imgproxy.transform",
		trace.WithAttributes(
			attribute.Int("imgproxy.width", params.Width),
			attribute.String("imgproxy.format", params.Format),
			attribute.Int("imgproxy.quality", params.Quality),
		),
	)
	defer span.End()

	url := buildProcessingURL(c.baseURL, sourceURL, params)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		err = fmt.Errorf("imgproxy: build request: %w", err)
		span.RecordError(err)
		return nil, "", err
	}

	if id := requestid.FromContext(ctx); id != "" {
		req.Header.Set("X-Request-Id", id)
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		metrics.ObserveImgproxy("error", elapsed.Seconds())
		err = fmt.Errorf("imgproxy: request failed: %w", err)
		span.RecordError(err)
		return nil, "", err
	}

	if resp.StatusCode != http.StatusOK {
		metrics.ObserveImgproxy(imgproxyStatusClass(resp.StatusCode), elapsed.Seconds())
		snippet := make([]byte, 256)
		n, _ := resp.Body.Read(snippet)
		resp.Body.Close()
		err = fmt.Errorf("imgproxy: unexpected status %d: %s", resp.StatusCode, snippet[:n])
		span.SetStatus(codes.Error, fmt.Sprintf("unexpected status %d", resp.StatusCode))
		span.RecordError(err)
		return nil, "", err
	}

	metrics.ObserveImgproxy("2xx", elapsed.Seconds())
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

func imgproxyStatusClass(code int) string {
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
