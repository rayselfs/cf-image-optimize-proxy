package handler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/cache"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/coalesce"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/imgproxy"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/metrics"
	"github.com/rayselfs/cf-image-optimize-proxy/internal/upstream"
)

var copyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024) // 32 KB buffer
		return &b
	},
}

// maxBodyBytes caps memory buffering for transform results and imgproxy fallback bodies.
// Non-image bypass responses are always streamed and are not subject to this limit.
const maxBodyBytes int64 = 100 * 1024 * 1024 // 100 MiB

// Handler is the main image optimization HTTP handler.
type Handler struct {
	Cache          cache.Cache
	Transformer    imgproxy.Transformer
	Resolver       upstream.Resolver
	Coalescer      coalesce.Coalescer
	MaxWidth       int
	DefaultQuality int
}

type processResult struct {
	body        []byte // non-nil only on fetchOriginalFallback path
	contentType string
	cacheStatus string
}

// New creates a new Handler.
func New(c cache.Cache, t imgproxy.Transformer, r upstream.Resolver, coal coalesce.Coalescer, maxWidth int, defaultQuality int) *Handler {
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

	sourceURL, headFunc, fetchFunc, err := h.Resolver.Resolve(r)
	if err != nil {
		slog.Error("handler: resolve", "error", err)
		h.writeError(w, err)
		return
	}

	headContentType, headErr := headFunc()
	if headErr != nil {
		slog.Error("handler: HEAD upstream failed", "error", headErr, "path", r.URL.Path)
		h.writeError(w, headErr)
		return
	}

	// Non-image content: stream directly without buffering.
	// Handles arbitrarily large files (video, PDF, ZIP) without OOM risk.
	if headContentType != "" && !strings.HasPrefix(headContentType, "image/") {
		h.streamBypass(w, r, fetchFunc, headContentType)
		return
	}

	value, err, _ := h.Coalescer.Do(r.Context(), key, func() (interface{}, error) {
		return h.processImage(r.Context(), r.URL.Path, sourceURL, fetchFunc, key, params)
	})
	if err != nil {
		slog.Error("handler: process image", "error", err)
		h.writeError(w, err)
		return
	}

	result, ok := value.(processResult)
	if !ok {
		slog.Error("handler: unexpected coalescer result type", "type", fmt.Sprintf("%T", value))
		h.writeError(w, fmt.Errorf("unexpected coalescer result type: %T", value))
		return
	}

	// Fallback path: body already in memory (imgproxy failed, served original).
	if result.body != nil {
		h.writeResult(w, result)
		return
	}

	// Success path: image was streamed into S3; read back and forward to client.
	cached, _, err := h.Cache.Get(r.Context(), key)
	if err != nil {
		slog.Error("handler: cache get after put", "key_hash", cacheKeyHash(key), "error", err)
		h.writeError(w, err)
		return
	}
	defer cached.Close()
	h.streamResponse(w, cached, result.contentType, result.cacheStatus)
}

func (h *Handler) streamResponse(w http.ResponseWriter, body io.Reader, contentType, cacheStatus string) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	w.Header().Set("X-Cache", cacheStatus)
	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)
	if _, err := io.CopyBuffer(w, body, *bufPtr); err != nil {
		slog.Debug("handler: write response", "error", err)
	}
}

func cacheKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:6])
}

// passThrough streams the request to the upstream without transformation.
// Used when no image transform params (imwidth/f/q) are present.
func (h *Handler) passThrough(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadline(w)

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

// streamBypass streams a non-image response directly to the client using io.CopyBuffer.
// No upper size limit is applied; content is never fully loaded into memory.
// The server WriteTimeout is cleared so arbitrarily large files (e.g. 10 GB video) are
// not interrupted mid-transfer.
func (h *Handler) streamBypass(w http.ResponseWriter, r *http.Request, fetchFunc func() (io.ReadCloser, string, error), headContentType string) {
	clearWriteDeadline(w)

	body, contentType, err := fetchFunc()
	if err != nil {
		slog.Error("handler: fetch bypass", "error", err, "path", r.URL.Path)
		h.writeError(w, err)
		return
	}
	defer body.Close()

	ct := contentType
	if ct == "" {
		ct = headContentType
	}
	if ct != "" {
		if err := validatePassThroughContentType(ct); err != nil {
			slog.Error("handler: invalid bypass content type", "error", err)
			h.writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", ct)
	}

	w.Header().Set("X-Cache", "BYPASS")
	metrics.IncCacheBypass()
	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)
	if _, err := io.CopyBuffer(w, body, *bufPtr); err != nil {
		slog.Error("handler: stream bypass write", "error", err, "path", r.URL.Path)
	}
}

func clearWriteDeadline(w http.ResponseWriter) {
	// Clear the per-response write deadline; the global WriteTimeout would otherwise
	// cut off large pass-through transfers after 60 s.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}
}

// processImage transforms an image via imgproxy and stores the result in the cache.
// Called inside the coalescer so concurrent cache-miss requests for the same key
// share a single transform operation.
func (h *Handler) processImage(ctx context.Context, urlPath, sourceURL string, fetchFunc func() (io.ReadCloser, string, error), key string, params *ImageParams) (processResult, error) {
	transformedBody, transformedContentType, err := h.Transformer.Transform(ctx, sourceURL, imgproxy.TransformParams{
		Width:   params.Width,
		Format:  params.Format,
		Quality: params.Quality,
	})
	if err != nil {
		slog.Error("handler: transform failed, fetching original fallback", "error", err, "path", urlPath)
		metrics.IncImgproxyError()
		return h.fetchOriginalFallback(fetchFunc)
	}
	defer transformedBody.Close()

	if err := validateTransformedContentType(transformedContentType); err != nil {
		slog.Error("handler: imgproxy returned invalid content type", "error", err, "path", urlPath)
		metrics.IncImgproxyError()
		return h.fetchOriginalFallback(fetchFunc)
	}

	pr, pw := io.Pipe()
	go func() {
		bufPtr := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufPtr)
		_, copyErr := io.CopyBuffer(pw, transformedBody, *bufPtr)
		pw.CloseWithError(copyErr)
	}()

	if err := h.Cache.Put(ctx, key, pr, transformedContentType); err != nil {
		_ = pr.Close() // unblock writer goroutine if Put failed before draining pipe
		slog.Error("handler: cache put", "key_hash", cacheKeyHash(key), "error", err)
		return processResult{}, err
	}

	metrics.IncCacheMiss()
	return processResult{contentType: transformedContentType, cacheStatus: "MISS"}, nil
}

// fetchOriginalFallback fetches the unmodified upstream image when imgproxy transform fails.
func (h *Handler) fetchOriginalFallback(fetchFunc func() (io.ReadCloser, string, error)) (processResult, error) {
	body, contentType, err := fetchFunc()
	if err != nil {
		return processResult{}, err
	}
	defer body.Close()
	if strings.ContainsAny(contentType, "\r\n") {
		return processResult{}, fmt.Errorf("upstream returned content type with illegal control characters")
	}
	data, err := h.readBody(body)
	if err != nil {
		return processResult{}, err
	}
	metrics.IncCacheMiss()
	return processResult{body: data, contentType: contentType, cacheStatus: "MISS"}, nil
}

func (h *Handler) readBody(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes limit", maxBodyBytes)
	}
	return data, nil
}

func (h *Handler) writeResult(w http.ResponseWriter, result processResult) {
	if result.contentType != "" {
		w.Header().Set("Content-Type", result.contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	w.Header().Set("X-Cache", result.cacheStatus)
	if _, err := w.Write(result.body); err != nil {
		slog.Debug("handler: write result", "error", err)
	}
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
