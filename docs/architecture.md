# Architecture: CloudFront Function ↔ Image Optimize Proxy

## Overview

The image optimization pipeline has two stages:
1. **CF Function (viewer-request)**: `imageOptimize` behavior normalizes query parameters
2. **Image Optimize Proxy (origin)**: Reads normalized params, transforms images via imgproxy

## CF Function Contract

The `imageOptimize` CF Function behavior (in `src/behaviors/image-optimize.ts`) normalizes the following query parameters before the request reaches the proxy:

| Param | Source | Output |
|-------|--------|--------|
| `imwidth` | `imwidth` query param or `CloudFront-Viewer-Width` header | Snapped to nearest ceiling breakpoint (e.g., 320, 640, 960, 1280, 1920) |
| `f` | `imformat` query param or `Accept` header negotiation | One of: `avif`, `webp`, `jpeg` |
| `q` | Existing `q`/`quality` param or default | Integer 1-100 (default: 75) |

## Proxy Reads

The proxy extracts these normalized params from the query string:
- `imwidth` → target width for resize
- `f` → output format for imgproxy
- `q` → quality setting

If none of these params are present, the proxy passes the request through without transformation.

## S3 Cache Key Format

```
{host}/{uri-path}/{imwidth}_{f}_{q}
```

Example: `stream.viverse.com/assets/hero-banner/640_webp_75`

## Format Negotiation

Format negotiation happens entirely at the CF Function level based on the browser's `Accept` header:
- `Accept: image/avif,...` → `f=avif`
- `Accept: image/webp,...` → `f=webp`
- Neither → `f=jpeg`

The proxy does NOT inspect the `Accept` header — it trusts the `f` param set by the CF Function.

## Source Identification

The proxy identifies upstream source type via request headers (injected by the CF Function `imageOptimize` behavior, or set as CloudFront origin custom headers in Terraform):
- `x-img-source-type: s3` + `x-img-source-bucket: <bucket>` → fetch from S3
- any other `x-img-source-type` value **or absent** → fetch via `x-img-upstream-gateway` URL (required; absent → error)

These headers are required for **all** requests — the proxy always resolves the upstream source, even in pass-through mode (when `imwidth`/`f`/`q` params are absent). If neither the CF Function `origin` option nor Terraform origin custom headers supply them, every request will error.

## No Changes Required

The existing `imageOptimize` behavior works as-is for the proxy architecture. No modifications needed.
