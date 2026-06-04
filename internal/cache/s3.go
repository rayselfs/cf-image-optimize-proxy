package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/metrics"
	"github.com/rayselfs/cloudfront-image-optimize-proxy/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Cache is the interface for image cache operations.
type Cache interface {
	Get(ctx context.Context, key string) (io.ReadCloser, string, error)
	Put(ctx context.Context, key string, body io.Reader, contentType string) error
}

// ErrNotFound is returned when a cache key does not exist.
var ErrNotFound = errors.New("cache: not found")

// ImageParams holds the image transformation parameters used to build a cache key.
type ImageParams struct {
	Width   int
	Format  string
	Quality int
}

// KeyFromRequest builds a cache key from host, path, and image params.
// Format: {host}/{path}/{width}_{format}_{quality}
func KeyFromRequest(host, path string, params ImageParams) string {
	path = strings.TrimPrefix(path, "/")
	return fmt.Sprintf("%s/%s/%d_%s_%d", host, path, params.Width, params.Format, params.Quality)
}

// S3API is the subset of the AWS S3 client used by S3Cache (enables mocking).
type S3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
}

// s3Uploader is the interface used for multipart uploads (enables mocking).
type s3Uploader interface {
	Upload(ctx context.Context, input *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

// S3Cache implements Cache backed by an S3 bucket.
type S3Cache struct {
	client   S3API
	bucket   string
	uploader s3Uploader
}

// NewS3Cache creates a new S3Cache.
func NewS3Cache(client S3API, bucket string) *S3Cache {
	return &S3Cache{client: client, bucket: bucket}
}

func NewS3CacheWithMultipart(client S3API, bucket string, uploader s3Uploader, multipartThreshold int64) *S3Cache {
	return &S3Cache{
		client:   client,
		bucket:   bucket,
		uploader: uploader,
	}
}

func (c *S3Cache) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	ctx, span := tracing.Tracer().Start(ctx, "s3.cache.get")
	defer span.End()

	start := time.Now()
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			span.SetAttributes(attribute.String("s3.outcome", "miss"))
			metrics.ObserveS3Get("miss", time.Since(start).Seconds())
			return nil, "", ErrNotFound
		}
		span.SetAttributes(attribute.String("s3.outcome", "error"))
		span.RecordError(err)
		metrics.ObserveS3Get("error", time.Since(start).Seconds())
		return nil, "", err
	}
	span.SetAttributes(attribute.String("s3.outcome", "hit"))
	metrics.ObserveS3Get("hit", time.Since(start).Seconds())

	contentType := ""
	if out.ContentType != nil {
		contentType = *out.ContentType
	}
	return out.Body, contentType, nil
}

func (c *S3Cache) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	ctx, span := tracing.Tracer().Start(ctx, "s3.cache.put")
	defer span.End()

	if strings.ContainsAny(contentType, "\r\n") {
		err := fmt.Errorf("cache: content type contains illegal control characters")
		span.SetStatus(codes.Error, "invalid content type")
		span.RecordError(err)
		return err
	}

	input := &s3.PutObjectInput{
		Bucket:       aws.String(c.bucket),
		Key:          aws.String(key),
		Body:         body,
		ContentType:  aws.String(contentType),
		CacheControl: aws.String("public, max-age=31536000"),
	}

	start := time.Now()
	_, seekable := body.(io.Seeker)
	if c.uploader != nil && !seekable {
		if _, err := c.uploader.Upload(ctx, input); err != nil {
			err = fmt.Errorf("cache: multipart upload: %w", err)
			span.RecordError(err)
			metrics.ObserveS3Put("error", time.Since(start).Seconds())
			return err
		}
	} else {
		if _, err := c.client.PutObject(ctx, input); err != nil {
			err = fmt.Errorf("cache: put object: %w", err)
			span.RecordError(err)
			metrics.ObserveS3Put("error", time.Since(start).Seconds())
			return err
		}
	}
	metrics.ObserveS3Put("success", time.Since(start).Seconds())
	return nil
}
