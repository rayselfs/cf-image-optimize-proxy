# AGENTS.md

Project-specific behavioral guidelines for AI agents working on this codebase.
Extends the global `~/.config/opencode/AGENTS.md`.

## Project Overview

**cf-image-optimize-proxy** is a Go reverse proxy that performs on-demand image
transformation via an external imgproxy service and caches results in S3.

```
CloudFront → proxy(:8080) → S3 cache hit  → return cached
                           → S3 cache miss → upstream resolve
                                           → imgproxy transform
                                           → store S3
                                           → return
```

CloudFront (or its Function) normalizes `imwidth`, `f`, and `q` query params before the request
reaches the proxy, and injects `X-Img-Source-Type` / `X-Img-Source-Bucket` /
`X-Img-Upstream-Gateway` origin custom headers. See `docs/architecture.md` for the full contract.

## Tech Stack

- **Language**: Go 1.25
- **Key dependencies**: `aws-sdk-go-v2` (S3 cache + presign), `golang.org/x/sync` (coalescing)
- **Image processing**: external imgproxy service (`darthsim/imgproxy`), URL configured via `IMGPROXY_URL`
- **Cache backend**: AWS S3
- **Deployment**: Kubernetes via Helm chart (`charts/cf-image-optimize-proxy/`)
- **Container**: Multi-stage Docker build → `distroless/static-debian12:nonroot`
- **CI (Azure DevOps)**: `azure-pipelines.yml`
- **CI (GitHub)**: `.github/workflows/ci.yml` (PR checks), `.github/workflows/release.yml` (build/publish)

## Project Structure

```
cmd/server/main.go               # Entry point — wires all dependencies, starts HTTP server
internal/
  config/config.go               # Env-based config (Load → *Config); defaults differ from Helm overrides
  handler/handler.go             # Core HTTP handler: cache check → transform → store → respond
  handler/params.go              # Query param parsing (imwidth, f, q)
  cache/s3.go                    # S3 cache: Get/Put with cache key derivation
  imgproxy/client.go             # imgproxy HTTP client — builds processing URL
  upstream/resolver.go           # Source resolver: S3 presign OR upstream gateway fetch
  coalesce/coalesce.go           # Request coalescing — dedup inflight transforms by cache key
  middleware/logging.go          # Structured JSON logging middleware
charts/cf-image-optimize-proxy/     # Helm chart for Kubernetes deployment
docs/architecture.md             # CloudFront ↔ proxy contract (cache key, format negotiation)
```

## Environment Variables

| Var               | Code default     | Helm override      | Description                   |
| ----------------- | ---------------- | ------------------ | ----------------------------- |
| `CACHE_S3_BUCKET` | — (**required**) | set at deploy time | S3 bucket for cached images   |
| `CACHE_S3_REGION` | `us-west-2`      | `us-east-1`        | AWS region of the S3 bucket   |
| `LISTEN_ADDR`     | `:9999`          | `:8080`            | HTTP server listen address    |
| `MAX_WIDTH`       | `1920`           | `1920`             | Maximum image width in pixels |
| `IMGPROXY_URL`    | — (**required**) | set at deploy time | External imgproxy service URL |

> The Helm chart's ConfigMap overrides `LISTEN_ADDR` and `CACHE_S3_REGION` from their code defaults.
> When running locally (without Helm), the code defaults apply.

## Request Headers (injected by CloudFront)

| Header                   | Required    | Description                                            |
| ------------------------ | ----------- | ------------------------------------------------------ |
| `X-Img-Source-Type`      | conditional | `s3` → fetch from S3; absent → use upstream gateway    |
| `X-Img-Source-Bucket`    | when `s3`   | S3 bucket containing the source image                  |
| `X-Img-Upstream-Gateway` | when non-s3 | Upstream gateway URL; **required** for non-S3 requests |

## Query Params (normalized by CloudFront Function)

| Param     | Example | Description                                                      |
| --------- | ------- | ---------------------------------------------------------------- |
| `imwidth` | `640`   | Target width (snapped to breakpoints: 320, 640, 960, 1280, 1920) |
| `f`       | `webp`  | Output format (`avif`, `webp`, `jpeg`)                           |
| `q`       | `75`    | Quality (1–100, default 75)                                      |

If none of these params are present, the proxy passes the request through without transformation.

## Code Conventions

- **Error handling**: Return errors up; don't silently swallow. Handler writes `http.Error` for user-facing errors.
- **Logging**: `log/slog` with structured JSON. Pattern: `slog.Error("msg", "error", err, "key", val)`.
- **Testing**: Table-driven tests using `t.Run`. Mock external deps via interfaces.
- **No global state**: All dependencies injected through constructor functions (`New(...)`, `NewClient(...)`, etc.).
- **Context propagation**: `context.Context` is the first argument in call chains.
- **No panics in handlers**: Use error returns; panics in HTTP handlers cause silent 500s in production.

## Development Commands

```bash
make test    # go test ./... -v -cover -race
make build   # go build -o bin/cf-image-optimize-proxy ./cmd/server/
make lint    # go vet ./...
make docker  # docker build -t cf-image-optimize-proxy:dev .
```

## Key Invariants (DO NOT break)

1. **Cache key format** (`{host}/{uri-path}/{imwidth}_{f}_{q}`): changing this invalidates
   all cached images in production S3.
2. **Request coalescing**: The coalescer deduplicates inflight requests for the same cache key.
   Breaking this causes thundering-herd under cache miss.
3. **Pass-through for no-transform**: If `imwidth`/`f`/`q` are absent, the proxy streams the
   original image without calling imgproxy. Preserve this behavior.
4. **imgproxy fallback**: If imgproxy fails, the handler falls back to serving the unoptimized
   original image. Preserve this fallback.
5. **Graceful shutdown**: Server has a 10-second shutdown window. Avoid blocking operations
   that outlast it.

## Testing

```bash
make test
```

Integration tests live in `internal/handler/integration_test.go` — they start a real HTTP server
and verify end-to-end flow using `net/http/httptest` to mock S3 and imgproxy.

## Deployment Notes

- Requires IRSA (IAM Roles for Service Accounts) for S3 access in EKS.
- imgproxy runs as an **external service** reachable at `IMGPROXY_URL` (not a sidecar).
- S3 bucket must exist in `CACHE_S3_REGION` before deployment.
- Docker images published to `ghcr.io/{owner}/{repo}` via GitHub Actions `release.yml`.
