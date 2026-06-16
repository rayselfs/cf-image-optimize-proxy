.PHONY: build test test-go test-helm lint lint-helm package-chart docker

HELM_CHART := charts/cf-image-optimize-proxy
HELM_UNITTEST_VERSION := v1.0.2
HELM_REQUIRED_VALUES := \
	--set config.cacheS3Bucket=cache-bucket \
	--set config.imgproxyURL=http://imgproxy.default.svc.cluster.local \
	--set config.allowedUpstreamGateways[0]=images.example.com \
	--set config.allowedSourceBuckets[0]=source-bucket

build:
	go build -o bin/cf-image-optimize-proxy ./cmd/server/

test: test-go test-helm

test-go:
	go test ./... -v -cover -race

test-helm:
	@if ! helm unittest --help >/dev/null 2>&1; then \
		echo "Installing helm-unittest plugin $(HELM_UNITTEST_VERSION)..."; \
		helm plugin uninstall unittest >/dev/null 2>&1 || true; \
		helm plugin install https://github.com/helm-unittest/helm-unittest.git --version $(HELM_UNITTEST_VERSION); \
	fi
	helm unittest $(HELM_CHART)

lint: lint-helm
	go vet ./...

lint-helm:
	helm lint $(HELM_CHART) $(HELM_REQUIRED_VALUES)
	helm template image-proxy $(HELM_CHART) $(HELM_REQUIRED_VALUES) >/dev/null

package-chart:
	helm package $(HELM_CHART) --destination dist

docker:
	docker build -t cf-image-optimize-proxy:dev .
