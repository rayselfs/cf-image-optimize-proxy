# cf-image-optimize-proxy

Go reverse proxy that transforms images on demand using an external imgproxy service and caches results in S3.

## Request Flow

```
CloudFront → proxy(:8080) → S3 cache hit  → return cached
                           → S3 cache miss → upstream resolve
                                           → imgproxy transform
                                           → store S3
                                           → return
```

CloudFront (or its Function) normalizes `imwidth`, `f`, and `q` query params before the request
reaches the proxy, and injects `X-Img-Source-Type` / `X-Img-Source-Bucket` /
`X-Img-Upstream-Gateway` origin custom headers to describe the image source.

See [`docs/architecture.md`](docs/architecture.md) for the full CloudFront ↔ proxy contract.

## Configuration

| Env var           | Default      | Helm override      | Description                             |
| ----------------- | ------------ | ------------------ | --------------------------------------- |
| `CACHE_S3_BUCKET` | **required** | set at deploy time | S3 bucket for cached transformed images |
| `CACHE_S3_REGION` | `us-west-2`  | `us-east-1`        | AWS region of the S3 bucket             |
| `LISTEN_ADDR`     | `:9999`      | `:8080`            | Proxy listen address                    |
| `MAX_WIDTH`       | `1920`       | `1920`             | Maximum allowed image width in pixels   |
| `IMGPROXY_URL`    | **required** | set at deploy time | External imgproxy service URL           |

> Code defaults apply when running locally. The Helm chart's ConfigMap overrides `LISTEN_ADDR`
> to `:8080` and `CACHE_S3_REGION` to `us-east-1` at deploy time.

### Request Headers (set by CloudFront)

| Header                   | Required      | Description                                                            |
| ------------------------ | ------------- | ---------------------------------------------------------------------- |
| `X-Img-Source-Type`      | optional      | `s3` → fetch from S3; any other value or absent → use upstream gateway |
| `X-Img-Source-Bucket`    | when `s3`     | S3 bucket containing the source image                                  |
| `X-Img-Upstream-Gateway` | when non-`s3` | Upstream gateway URL; **required** when not using S3                   |

### Query Params (normalized by CloudFront Function)

| Param     | Example | Description                                                                  |
| --------- | ------- | ---------------------------------------------------------------------------- |
| `imwidth` | `640`   | Target width — snapped to nearest ceiling breakpoint (320/640/960/1280/1920) |
| `f`       | `webp`  | Output format (`avif`, `webp`, `jpeg`)                                       |
| `q`       | `75`    | Quality (1–100; default 75)                                                  |

If none of these params are present, the proxy passes the request through without transformation.

## Development

Requirements: Go 1.25+

```bash
make test     # go test ./... -v -cover -race
make build    # go build -o bin/cf-image-optimize-proxy ./cmd/server/
make lint     # go vet ./... and helm lint/template
make docker   # docker build -t cf-image-optimize-proxy:dev .
```

Integration tests in `internal/handler/integration_test.go` start a real HTTP server and mock
S3/imgproxy responses via `net/http/httptest`.

## Deployment

Requires an external [imgproxy](https://imgproxy.net/) service reachable at `IMGPROXY_URL`.

### Prerequisites

**IRSA (IAM Roles for Service Accounts)** is required for the proxy to read/write the S3 cache
bucket. Create an IAM role with the following policy and annotate the ServiceAccount via
`serviceAccount.roleArn`.

Minimum IAM policy for the cache bucket:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject"],
      "Resource": "arn:aws:s3:::<cache-bucket>/*"
    }
  ]
}
```

> If the source images are also in S3 (i.e. `X-Img-Source-Type: s3` requests), the proxy
> presigns GetObject URLs for the source bucket using the same role. Add a separate statement:
>
> ```json
> {
>   "Effect": "Allow",
>   "Action": "s3:GetObject",
>   "Resource": "arn:aws:s3:::<source-bucket>/*"
> }
> ```

### Deploying on Kubernetes

Use the Helm chart in `charts/cf-image-optimize-proxy`. Configure S3 access via IRSA and set the required runtime values:

```bash
helm upgrade --install image-proxy charts/cf-image-optimize-proxy \
  --namespace image-proxy --create-namespace \
  --set config.cacheS3Bucket=<cache-bucket> \
  --set config.imgproxyURL=http://imgproxy.default.svc.cluster.local \
  --set config.allowedUpstreamGateways[0]=images.example.com \
  --set config.allowedSourceBuckets[0]=source-bucket \
  --set serviceAccount.annotations.eks\.amazonaws\.com/role-arn=<irsa-role-arn>
```

If `CF_ORIGIN_SECRET` is created by a controller such as External Secrets Operator, render that resource through `extraManifests` and point the deployment at the generated Secret with `existingSecret.name`. The generated Secret must contain a `CF_ORIGIN_SECRET` key:

```yaml
existingSecret:
  name: image-proxy-origin-secret

extraManifests:
  - apiVersion: external-secrets.io/v1
    kind: ExternalSecret
    metadata:
      name: '{{ include "cf-image-optimize-proxy.fullname" . }}-origin'
    spec:
      refreshInterval: 1h
      secretStoreRef:
        kind: ClusterSecretStore
        name: aws-secrets-manager
      target:
        name: image-proxy-origin-secret
      data:
        - secretKey: CF_ORIGIN_SECRET
          remoteRef:
            key: cloudfront/origin-secret
```

### Deploying on AWS Lambda

Use the Lambda image (`-lambda` suffix). The image bundles [AWS Lambda Web Adapter](https://github.com/awslabs/aws-lambda-web-adapter) which translates Lambda invocations into HTTP requests to the proxy.

**Lambda function configuration:**

| Env var                        | Value             | Notes                                             |
| ------------------------------ | ----------------- | ------------------------------------------------- |
| `AWS_LAMBDA_EXEC_WRAPPER`      | `/lambda-adapter` | Activates Lambda Web Adapter                      |
| `AWS_LWA_PORT`                 | `8080`            | Port the adapter forwards to                      |
| `AWS_LWA_READINESS_CHECK_PATH` | `/health`         | Cold-start health check (avoids probing imgproxy) |
| `AWS_LWA_INVOKE_MODE`          | `response_stream` | Required for large file passthrough               |
| `LISTEN_ADDR`                  | `:8080`           | Must match `AWS_LWA_PORT`                         |
| `CACHE_S3_BUCKET`              | `<bucket>`        |                                                   |
| `IMGPROXY_URL`                 | `<url>`           |                                                   |
| `ALLOWED_UPSTREAM_GATEWAYS`    | `<csv>`           |                                                   |
| `ALLOWED_SOURCE_BUCKETS`       | `<csv>`           |                                                   |

**Lambda Function URL:**

- `InvokeMode: RESPONSE_STREAM` — required for streaming large files to CloudFront
- `AuthType: NONE` — CloudFront origin verification is handled by `CF_ORIGIN_SECRET`

**CloudFront origin:** point directly at the Lambda Function URL. Do not use API Gateway (6 MB response limit).

**IAM:** grant the Lambda execution role `s3:GetObject` + `s3:PutObject` on the cache bucket (same policy as IRSA above).

### Published artifacts (GitHub)

| Artifact              | Registry                                                     |
| --------------------- | ------------------------------------------------------------ |
| Docker image (K8s)    | `ghcr.io/{owner}/{repo}:{tag}`                               |
| Docker image (Lambda) | `ghcr.io/{owner}/{repo}-lambda:{tag}`                        |
| Helm chart            | `oci://ghcr.io/{owner}/charts/cf-image-optimize-proxy:{tag}` |
