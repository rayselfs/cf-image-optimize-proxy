package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	mu  sync.Mutex
	reg *prometheus.Registry

	requestsTotal         prometheus.Counter
	cacheHits             prometheus.Counter
	cacheMisses           prometheus.Counter
	cacheBypasses         prometheus.Counter
	imgproxyErrors        prometheus.Counter
	putErrors             prometheus.Counter
	asyncCachePutInflight prometheus.Gauge
	asyncCachePutDropped  prometheus.Counter

	httpRequestDuration   *prometheus.HistogramVec
	imgproxyDuration      *prometheus.HistogramVec
	s3GetDuration         *prometheus.HistogramVec
	s3PutDuration         *prometheus.HistogramVec
	upstreamFetchDuration *prometheus.HistogramVec

	metricsHandler http.Handler
)

func init() {
	initRegistry()
}

func initRegistry() {
	reg = prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	requestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total image proxy requests",
	})
	cacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Cache HIT responses served",
	})
	cacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cache_misses_total",
		Help: "Cache MISS responses after transform",
	})
	cacheBypasses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cache_bypasses_total",
		Help: "Requests bypassed due to non-image content",
	})
	imgproxyErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "imgproxy_errors_total",
		Help: "imgproxy transform failures (fell back to original)",
	})
	putErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cache_put_errors_total",
		Help: "S3 cache write failures",
	})
	asyncCachePutInflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "async_cache_put_inflight",
		Help: "Number of async S3 cache puts currently in flight",
	})
	asyncCachePutDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "async_cache_put_dropped_total",
		Help: "Async S3 cache puts dropped due to full semaphore",
	})
	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "status_class", "cache_status"})
	imgproxyDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "imgproxy_duration_seconds",
		Help:    "imgproxy transform latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"status_class"})
	s3GetDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "s3_get_duration_seconds",
		Help:    "S3 cache GET latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})
	s3PutDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "s3_put_duration_seconds",
		Help:    "S3 cache PUT latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})
	upstreamFetchDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "upstream_fetch_duration_seconds",
		Help:    "Upstream fetch latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})

	reg.MustRegister(
		requestsTotal,
		cacheHits,
		cacheMisses,
		cacheBypasses,
		imgproxyErrors,
		putErrors,
		asyncCachePutInflight,
		asyncCachePutDropped,
		httpRequestDuration,
		imgproxyDuration,
		s3GetDuration,
		s3PutDuration,
		upstreamFetchDuration,
	)

	metricsHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// Reset re-creates the registry and all counters. Intended for use in tests only.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	initRegistry()
}

// Registry returns the package-level prometheus registry.
func Registry() *prometheus.Registry {
	mu.Lock()
	defer mu.Unlock()
	return reg
}

func IncRequest()               { requestsTotal.Inc() }
func IncCacheHit()              { cacheHits.Inc() }
func IncCacheMiss()             { cacheMisses.Inc() }
func IncCacheBypass()           { cacheBypasses.Inc() }
func IncImgproxyError()         { imgproxyErrors.Inc() }
func IncPutError()              { putErrors.Inc() }
func IncAsyncCachePutInflight() { asyncCachePutInflight.Inc() }
func DecAsyncCachePutInflight() { asyncCachePutInflight.Dec() }
func IncAsyncCachePutDropped()  { asyncCachePutDropped.Inc() }

// Handler returns an HTTP handler that serves Prometheus metrics.
func Handler() http.Handler {
	mu.Lock()
	h := metricsHandler
	mu.Unlock()
	return h
}

// ObserveHTTPRequest records an HTTP request duration sample.
func ObserveHTTPRequest(method, statusClass, cacheStatus string, seconds float64) {
	httpRequestDuration.WithLabelValues(method, statusClass, cacheStatus).Observe(seconds)
}

// ObserveImgproxy records an imgproxy transform duration sample.
func ObserveImgproxy(statusClass string, seconds float64) {
	imgproxyDuration.WithLabelValues(statusClass).Observe(seconds)
}

// ObserveS3Get records an S3 cache GET duration sample.
func ObserveS3Get(outcome string, seconds float64) {
	s3GetDuration.WithLabelValues(outcome).Observe(seconds)
}

// ObserveS3Put records an S3 cache PUT duration sample.
func ObserveS3Put(outcome string, seconds float64) {
	s3PutDuration.WithLabelValues(outcome).Observe(seconds)
}

// ObserveUpstreamFetch records an upstream fetch duration sample.
func ObserveUpstreamFetch(outcome string, seconds float64) {
	upstreamFetchDuration.WithLabelValues(outcome).Observe(seconds)
}
