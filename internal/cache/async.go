package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/metrics"
)

// keyHash returns the first 12 hex characters of the SHA-256 of key.
// Used to identify cache keys in logs without exposing path/PII.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:6]) // 6 bytes = 12 hex chars
}

// AsyncPutCache wraps a FileCache and executes Put operations in background goroutines.
// Use Wait to drain in-flight puts during graceful shutdown.
type AsyncPutCache struct {
	inner   FileCache
	timeout time.Duration
	sem     chan struct{}
	wg      sync.WaitGroup
}

// WrapAsyncPut wraps c so that Put is executed in a background goroutine.
// Concurrency is capped at maxConcurrency; excess puts are dropped and logged.
func WrapAsyncPut(c FileCache, timeout time.Duration, maxConcurrency int) *AsyncPutCache {
	return &AsyncPutCache{
		inner:   c,
		timeout: timeout,
		sem:     make(chan struct{}, maxConcurrency),
	}
}

func (a *AsyncPutCache) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	return a.inner.Get(ctx, key)
}

// Put fires a background goroutine to write body to the inner cache.
// Always returns nil immediately. body must be fully buffered (e.g. bytes.NewReader).
func (a *AsyncPutCache) Put(_ context.Context, key string, body io.Reader, contentType string) error {
	select {
	case a.sem <- struct{}{}:
	default:
		metrics.IncAsyncCachePutDropped()
		slog.Warn("async cache put dropped: worker pool full", "key_hash", keyHash(key))
		metrics.IncPutError()
		return nil
	}

	metrics.IncAsyncCachePutInflight()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() { <-a.sem }()
		defer metrics.DecAsyncCachePutInflight()

		putCtx, cancel := context.WithTimeout(context.Background(), a.timeout)
		defer cancel()
		if err := a.inner.Put(putCtx, key, body, contentType); err != nil {
			slog.Error("async cache put failed", "key_hash", keyHash(key), "error", err)
			metrics.IncPutError()
		}
	}()
	return nil
}

// PutFile delegates to the inner FileCache asynchronously.
// It uses the async worker pool; excess puts are dropped, logged, and the temp file is cleaned up.
func (a *AsyncPutCache) PutFile(ctx context.Context, key, filePath, contentType string) error {
	select {
	case a.sem <- struct{}{}:
	default:
		metrics.IncAsyncCachePutDropped()
		slog.Warn("async cache put file dropped: worker pool full", "key_hash", keyHash(key))
		metrics.IncPutError()
		_ = os.Remove(filePath)
		return nil
	}

	metrics.IncAsyncCachePutInflight()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() { <-a.sem }()
		defer metrics.DecAsyncCachePutInflight()
		defer os.Remove(filePath)

		putCtx, cancel := context.WithTimeout(context.Background(), a.timeout)
		defer cancel()

		if err := a.inner.PutFile(putCtx, key, filePath, contentType); err != nil {
			slog.Error("async cache put file failed", "key_hash", keyHash(key), "error", err)
			metrics.IncPutError()
		}
	}()
	return nil
}

// Wait blocks until all in-flight Put goroutines complete.
func (a *AsyncPutCache) Wait() {
	a.wg.Wait()
}

// WaitContext blocks until all in-flight Put goroutines complete or ctx is cancelled.
// Returns ctx.Err() if the context expires before all goroutines finish.
func (a *AsyncPutCache) WaitContext(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
