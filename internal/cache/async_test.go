package cache

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/metrics"
)

type syncCache struct {
	mu      sync.Mutex
	putKey  string
	putData []byte
	putCT   string
	putErr  error
	calls   int
}

func (s *syncCache) Get(_ context.Context, _ string) (io.ReadCloser, string, error) {
	return nil, "", ErrNotFound
}

func (s *syncCache) Put(_ context.Context, key string, body io.Reader, contentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.putKey = key
	s.putCT = contentType
	data, _ := io.ReadAll(body)
	s.putData = data
	return s.putErr
}

func (s *syncCache) PutFile(_ context.Context, key, _ string, contentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.putKey = key
	s.putCT = contentType
	return s.putErr
}

func TestWrapAsyncPutReturnsImmediately(t *testing.T) {
	inner := &syncCache{}
	wrapped := WrapAsyncPut(inner, 5*time.Second, 32)

	start := time.Now()
	err := wrapped.Put(context.Background(), "key", bytes.NewReader([]byte("data")), "image/webp")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}
	// The wrapper should return well under 1 second (not waiting for inner.Put).
	if elapsed > 500*time.Millisecond {
		t.Errorf("Put took %v, expected near-instant return", elapsed)
	}
}

func TestWrapAsyncPutDelegatesData(t *testing.T) {
	inner := &syncCache{}
	wrapped := WrapAsyncPut(inner, 5*time.Second, 32)

	data := []byte("hello-image")
	_ = wrapped.Put(context.Background(), "mykey", bytes.NewReader(data), "image/avif")

	// Wait for goroutine to complete via Wait().
	wrapped.Wait()

	inner.mu.Lock()
	defer inner.mu.Unlock()
	if inner.calls != 1 {
		t.Fatalf("inner.Put calls = %d, want 1", inner.calls)
	}
	if inner.putKey != "mykey" {
		t.Errorf("key = %q, want mykey", inner.putKey)
	}
	if !bytes.Equal(inner.putData, data) {
		t.Errorf("data = %q, want %q", inner.putData, data)
	}
	if inner.putCT != "image/avif" {
		t.Errorf("contentType = %q, want image/avif", inner.putCT)
	}
}

func TestWrapAsyncPutGetPassThrough(t *testing.T) {
	inner := &syncCache{}
	wrapped := WrapAsyncPut(inner, 5*time.Second, 32)

	_, _, err := wrapped.Get(context.Background(), "key")
	if err != ErrNotFound {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
}

func TestWrapAsyncPutWait(t *testing.T) {
	inner := &syncCache{}
	wrapped := WrapAsyncPut(inner, 5*time.Second, 32)

	const n = 5
	for i := 0; i < n; i++ {
		_ = wrapped.Put(context.Background(), "key", bytes.NewReader([]byte("data")), "image/webp")
	}

	// Wait must block until all goroutines complete.
	wrapped.Wait()

	inner.mu.Lock()
	calls := inner.calls
	inner.mu.Unlock()

	if calls != n {
		t.Errorf("inner.Put calls after Wait = %d, want %d", calls, n)
	}
}

func TestWrapAsyncPutCustomConcurrency(t *testing.T) {
	inner := &syncCache{}
	a := WrapAsyncPut(inner, 5*time.Second, 4)
	if cap(a.sem) != 4 {
		t.Fatalf("sem capacity = %d, want 4", cap(a.sem))
	}
	if a.timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s", a.timeout)
	}
}

func TestWrapAsyncPutDefaults(t *testing.T) {
	inner := &syncCache{}
	a := WrapAsyncPut(inner, 30*time.Second, 32)
	if cap(a.sem) != 32 {
		t.Fatalf("sem capacity = %d, want 32", cap(a.sem))
	}
	if a.timeout != 30*time.Second {
		t.Fatalf("timeout = %v, want 30s", a.timeout)
	}
}

func TestAsyncPutCacheKeyRedaction(t *testing.T) {
	// Fill the semaphore so the next Put hits the drop path.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	inner := &syncCache{}
	a := WrapAsyncPut(inner, 1*time.Second, 1)

	// Saturate the pool.
	a.sem <- struct{}{}

	sensitiveKey := "/private/user/email@example.com/640_webp_75"
	_ = a.Put(context.Background(), sensitiveKey, bytes.NewReader([]byte("data")), "image/webp")

	logOutput := buf.String()
	if strings.Contains(logOutput, sensitiveKey) {
		t.Fatalf("raw key leaked into log: %s", logOutput)
	}
	if !strings.Contains(logOutput, "key_hash") {
		t.Fatalf("key_hash attribute not found in log: %s", logOutput)
	}

	// Release the semaphore.
	<-a.sem
}

// blockingInnerCache blocks Put until its block channel is closed.
type blockingInnerCache struct {
	block chan struct{}
}

func (b *blockingInnerCache) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	return nil, "", ErrNotFound
}

func (b *blockingInnerCache) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	select {
	case <-b.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingInnerCache) PutFile(ctx context.Context, key, _ string, contentType string) error {
	select {
	case <-b.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestAsyncPutDroppedWhenPoolFull(t *testing.T) {
	// Pool capacity 1, semaphore pre-filled → next Put is dropped immediately.
	inner := &syncCache{}
	a := WrapAsyncPut(inner, 5*time.Second, 1)

	// Occupy the one slot.
	a.sem <- struct{}{}

	err := a.Put(context.Background(), "some/key", bytes.NewReader([]byte("body")), "image/webp")
	if err != nil {
		t.Fatalf("Put returned unexpected error: %v", err)
	}
	// Drop is silent (returns nil). Verify no goroutine was launched.
	// Wait should return immediately because no goroutine was added.
	done := make(chan struct{})
	go func() {
		a.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() blocked unexpectedly after dropped put")
	}

	// Release the manually held slot.
	<-a.sem
}

func TestAsyncPutTimeoutExpires(t *testing.T) {
	// Inner cache blocks until the context deadline; timeout must fire.
	block := make(chan struct{})
	blocking := &blockingInnerCache{block: block}
	a := WrapAsyncPut(blocking, 50*time.Millisecond, 2) // 50ms timeout

	if err := a.Put(context.Background(), "key1", bytes.NewReader([]byte("x")), "image/webp"); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	// Wait should return after the timeout fires (≤ ~500ms with margin).
	done := make(chan struct{})
	go func() {
		a.Wait()
		close(done)
	}()
	// Unblock the inner cache so the goroutine can exit.
	close(block)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait() did not return after timeout")
	}
}

func TestAsyncPutWaitDrainsAll(t *testing.T) {
	// All puts must finish before Wait returns.
	inner := &syncCache{}
	a := WrapAsyncPut(inner, 5*time.Second, 4)
	const n = 4
	for i := 0; i < n; i++ {
		body := bytes.NewReader([]byte("data"))
		if err := a.Put(context.Background(), "key", body, "image/webp"); err != nil {
			t.Fatalf("Put error: %v", err)
		}
	}
	a.Wait()
	// All puts completed; inner.calls should equal n (or ≥ n if some ran fast).
	// We just assert no panic and Wait returned.
	inner.mu.Lock()
	calls := inner.calls
	inner.mu.Unlock()
	if calls != n {
		t.Errorf("inner.Put calls after Wait = %d, want %d", calls, n)
	}
}

// slowCache blocks Put for the given duration before delegating to syncCache.
type slowCache struct {
	syncCache
	delay time.Duration
}

func (s *slowCache) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.syncCache.Put(ctx, key, body, contentType)
}

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				switch {
				case m.Gauge != nil:
					return m.Gauge.GetValue()
				case m.Counter != nil:
					return m.Counter.GetValue()
				}
			}
		}
	}
	return 0
}

func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				if m.Counter != nil {
					return m.Counter.GetValue()
				}
			}
		}
	}
	return 0
}

func TestInflightGaugeIncrementDecrement(t *testing.T) {
	metrics.Reset()
	inner := &slowCache{delay: 50 * time.Millisecond}
	a := WrapAsyncPut(inner, 5*time.Second, 4)

	if err := a.Put(context.Background(), "key", bytes.NewReader([]byte("data")), "image/webp"); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	if v := gaugeValue(t, "async_cache_put_inflight"); v != 1 {
		t.Errorf("inflight gauge = %v, want 1", v)
	}

	a.Wait()

	if v := gaugeValue(t, "async_cache_put_inflight"); v != 0 {
		t.Errorf("inflight gauge after Wait = %v, want 0", v)
	}
}

func TestDroppedCounterIncrements(t *testing.T) {
	metrics.Reset()
	block := make(chan struct{})
	blocking := &blockingInnerCache{block: block}
	a := WrapAsyncPut(blocking, 5*time.Second, 1)

	if err := a.Put(context.Background(), "key1", bytes.NewReader([]byte("x")), "image/webp"); err != nil {
		t.Fatalf("Put error: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	if err := a.Put(context.Background(), "key2", bytes.NewReader([]byte("y")), "image/webp"); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	if v := counterValue(t, "async_cache_put_dropped_total"); v != 1 {
		t.Errorf("dropped counter = %v, want 1", v)
	}

	close(block)
	a.Wait()
}

type fileSyncCache struct {
	syncCache
	putFileCalls int
	putFilePath  string
}

func (f *fileSyncCache) PutFile(ctx context.Context, key, filePath, contentType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.putFileCalls++
	f.putKey = key
	f.putFilePath = filePath
	f.putCT = contentType
	data, _ := os.ReadFile(filePath)
	f.putData = data
	return f.putErr
}

func TestWrapAsyncPutFileReturnsImmediately(t *testing.T) {
	inner := &fileSyncCache{}
	wrapped := WrapAsyncPut(inner, 5*time.Second, 32)

	tmpFile, err := os.CreateTemp("", "async-test-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	_, _ = tmpFile.Write([]byte("file-data"))
	_ = tmpFile.Close()

	start := time.Now()
	err = wrapped.PutFile(context.Background(), "key", tmpPath, "image/webp")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("PutFile returned error: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("PutFile took %v, expected near-instant return", elapsed)
	}

	wrapped.Wait()

	inner.mu.Lock()
	defer inner.mu.Unlock()
	if inner.putFileCalls != 1 {
		t.Fatalf("inner.PutFile calls = %d, want 1", inner.putFileCalls)
	}
	if !bytes.Equal(inner.putData, []byte("file-data")) {
		t.Errorf("data = %q, want file-data", inner.putData)
	}

	// Temp file should be removed by defer os.Remove in wrapped.PutFile
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file not deleted: %v", err)
	}
}

func TestWrapAsyncPutFileDroppedWhenPoolFull(t *testing.T) {
	inner := &fileSyncCache{}
	a := WrapAsyncPut(inner, 5*time.Second, 1)

	// Occupy the slot
	a.sem <- struct{}{}

	tmpFile, err := os.CreateTemp("", "async-test-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	_, _ = tmpFile.Write([]byte("file-data"))
	_ = tmpFile.Close()

	metrics.Reset()
	err = a.PutFile(context.Background(), "key", tmpPath, "image/webp")
	if err != nil {
		t.Fatalf("PutFile error on drop: %v", err)
	}

	if v := counterValue(t, "async_cache_put_dropped_total"); v != 1 {
		t.Errorf("dropped counter = %v, want 1", v)
	}

	// Temp file must be deleted immediately
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file not deleted on drop: %v", err)
	}

	<-a.sem
}
