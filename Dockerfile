# Build stage
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /main ./cmd/server/

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /main /main
EXPOSE 9999
ENTRYPOINT ["/main"]
