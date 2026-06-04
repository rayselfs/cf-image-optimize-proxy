package handler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/cache"
	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/coalesce"
	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/imgproxy"
	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/metrics"
	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/upstream"
)

var copyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024) // 32 KB buffer
		return &b
	},
}

// Handler is the main image optimization HTTP handler.
type Handler struct {
	Cache          cache.FileCache
	Transformer    imgproxy.Transformer
	Resolver       upstream.Resolver
	Coalescer      coalesce.Coalescer
	MaxWidth       int
	DefaultQuality int
}

type processResult struct {
	body        []byte
	contentType string
	cacheStatus string
}

// New creates a new Handler.
func New(c cache.FileCache, t imgproxy.Transformer, r upstream.Resolver, coal coalesce.Coalescer, maxWidth int, defaultQuality int) *Handler {
	return &Handler{
		Cache:          c,
		Transformer:    t,
		Resolver:       r,
		Coalescer:      coal,
		MaxWidth:       maxWidth,
		DefaultQuality: defaultQuality,
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	params := ParseParams(r.URL.Query(), h.DefaultQuality)
	if params == nil {
		h.passThrough(w, r)
		return
	}

	if h.MaxWidth > 0 && params.Width > h.MaxWidth {
		params.Width = h.MaxWidth
	}

	key := cache.KeyFromRequest(r.Host, r.URL.Path, cache.ImageParams{
		Width:   params.Width,
		Format:  params.Format,
		Quality: params.Quality,
	})

	if body, contentType, err := h.Cache.Get(r.Context(), key); err == nil {
		defer body.Close()
		h.streamResponse(w, body, contentType, "HIT")
		metrics.IncCacheHit()
		return
	} else if !errors.Is(err, cache.ErrNotFound) {
		slog.Error("handler: cache get", "key_hash", cacheKeyHash(key), "error", err)
	}

	value, err, _ := h.Coalescer.Do(r.Context(), key, func() (interface{}, error) {
		return h.process(r, key, params)
	})
	if err != nil {
		slog.Error("handler: process request", "error", err)
		h.writeError(w, err)
		return
	}

	result, ok := value.(processResult)
	if !ok {
		slog.Error("handler: unexpected coalescer result type", "type", fmt.Sprintf("%T", value))
		h.writeError(w, fmt.Errorf("unexpected coalescer result type: %T", value))
		return
	}

	h.writeResult(w, result)
}

func (h *Handler) streamResponse(w http.ResponseWriter, body io.Reader, contentType, cacheStatus string) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	w.Header().Set("X-Cache", cacheStatus)
	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)
	_, _ = io.CopyBuffer(w, body, *bufPtr)
}

func cacheKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:6])
}

func (h *Handler) passThrough(w http.ResponseWriter, r *http.Request) {
	_, _, fetchFunc, err := h.Resolver.Resolve(r)
	if err != nil {
		slog.Error("handler: resolve pass-through", "error", err)
		h.writeError(w, err)
		return
	}

	body, contentType, err := fetchFunc()
	if err != nil {
		slog.Error("handler: fetch pass-through", "error", err)
		h.writeError(w, err)
		return
	}
	defer body.Close()

	// Validate content type to prevent header injection.
	if contentType != "" {
		if err := validatePassThroughContentType(contentType); err != nil {
			slog.Error("handler: invalid pass-through content type", "error", err)
			h.writeError(w, err)
			return
		}
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)
	if _, err := io.CopyBuffer(w, body, *bufPtr); err != nil {
		slog.Error("handler: write pass-through", "error", err)
	}
}

func (h *Handler) process(r *http.Request, key string, params *ImageParams) (processResult, error) {
	sourceURL, headFunc, fetchFunc, err := h.Resolver.Resolve(r)
	if err != nil {
		return processResult{}, err
	}

	headContentType, headErr := headFunc()
	if headErr != nil {
		slog.Error("handler: HEAD upstream failed", "error", headErr, "path", r.URL.Path)
		return processResult{}, headErr
	}

	if headContentType != "" && !strings.HasPrefix(headContentType, "image/") {
		originalBody, originalContentType, err := fetchFunc()
		if err != nil {
			return processResult{}, err
		}
		defer originalBody.Close()
		if strings.ContainsAny(originalContentType, "\r\n") {
			return processResult{}, fmt.Errorf("upstream returned content type with illegal control characters")
		}
		originalData, err := h.readBody(originalBody)
		if err != nil {
			return processResult{}, err
		}
		metrics.IncCacheBypass()
		return processResult{body: originalData, contentType: originalContentType, cacheStatus: "BYPASS"}, nil
	}

	transformedBody, transformedContentType, err := h.Transformer.Transform(r.Context(), sourceURL, imgproxy.TransformParams{
		Width:   params.Width,
		Format:  params.Format,
		Quality: params.Quality,
	})
	if err != nil {
		slog.Error("handler: transform failed, fetching original fallback",
			"error", err,
			"path", r.URL.Path,
		)
		metrics.IncImgproxyError()
		originalBody, originalContentType, fetchErr := fetchFunc()
		if fetchErr != nil {
			return processResult{}, fetchErr
		}
		defer originalBody.Close()
		originalData, err := h.readBody(originalBody)
		if err != nil {
			return processResult{}, err
		}
		metrics.IncCacheMiss()
		return processResult{body: originalData, contentType: originalContentType, cacheStatus: "MISS"}, nil
	}
	defer transformedBody.Close()

	if err := validateTransformedContentType(transformedContentType); err != nil {
		slog.Error("handler: imgproxy returned invalid content type",
			"error", err,
			"path", r.URL.Path,
		)
		metrics.IncImgproxyError()
		originalBody, originalContentType, fetchErr := fetchFunc()
		if fetchErr != nil {
			return processResult{}, fetchErr
		}
		defer originalBody.Close()
		originalData, err := h.readBody(originalBody)
		if err != nil {
			return processResult{}, err
		}
		metrics.IncCacheMiss()
		return processResult{body: originalData, contentType: originalContentType, cacheStatus: "MISS"}, nil
	}

	tmpFile, err := os.CreateTemp("", "image-optimize-proxy-*")
	if err != nil {
		return processResult{}, fmt.Errorf("handler: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)
	_, err = io.CopyBuffer(tmpFile, transformedBody, *bufPtr)
	if closeErr := tmpFile.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		_ = os.Remove(tmpPath)
		return processResult{}, fmt.Errorf("handler: write temp file: %w", err)
	}

	bodyBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return processResult{}, fmt.Errorf("handler: read temp file: %w", err)
	}

	if err := h.Cache.PutFile(context.Background(), key, tmpPath, transformedContentType); err != nil {
		slog.Error("handler: cache put file", "key_hash", cacheKeyHash(key), "error", err)
		_ = os.Remove(tmpPath)
		return processResult{}, err
	}

	metrics.IncCacheMiss()
	return processResult{body: bodyBytes, contentType: transformedContentType, cacheStatus: "MISS"}, nil
}

func (h *Handler) readBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func (h *Handler) writeResult(w http.ResponseWriter, result processResult) {
	if result.contentType != "" {
		w.Header().Set("Content-Type", result.contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	w.Header().Set("X-Cache", result.cacheStatus)
	_, _ = w.Write(result.body)
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	var statusErr *upstream.StatusError
	if errors.As(err, &statusErr) {
		if statusErr.Code >= 400 && statusErr.Code < 500 {
			http.Error(w, http.StatusText(statusErr.Code), statusErr.Code)
			return
		}
	}
	http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
}
