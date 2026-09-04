.PHONY: lint test unit-test integration-test envtest-assets distro-test dashboard-check build clean buildx-builder docker-build docker-verify

BINARY  := crossplane-state-metrics
CHART   := charts/crossplane-state-metrics
VERSION ?= dev

# Kubernetes version the integration tests run their API server at.
ENVTEST_K8S_VERSION ?= 1.37.0

# Buildx builder used for multi-arch work. Docker's default builder uses the
# plain "docker" driver, which can build neither a manifest list nor an OCI
# export; both need the docker-container driver.
BUILDX_BUILDER ?= $(BINARY)-builder

# lint runs the custom namedreturns linter first, then golangci-lint. Sequential
# and fail-fast: if named returns are wrong there is no point linting further.
# The integration build tag is set for both linters. A build-tagged file is not
# part of the default build, so without it the integration tests would ship
# without ever being linted.
lint:
	@echo "Running namedreturns linter..."
	GOFLAGS=-tags=integration namedreturns ./...
	@echo "Running golangci-lint..."
	golangci-lint run

# test is the single "run everything" target: unit tests, integration tests
# against a real API server, AND the distro renders. A build that never renders
# the distribution, or never talks to a real API server, is not a green build.
test: unit-test integration-test dashboard-check distro-test

unit-test:
	go test -race -cover ./...

# envtest-assets downloads the kube-apiserver and etcd binaries the integration
# tests run against. Safe to re-run; it no-ops when they are already present.
envtest-assets:
	@command -v setup-envtest >/dev/null 2>&1 || \
		go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@setup-envtest use $(ENVTEST_K8S_VERSION) -p path >/dev/null

# integration-test runs the exporter against a real kube-apiserver: real CRDs
# installed, real objects created, real informers, real scrape output. The unit
# tests use a fake client, which proves the logic but not the wiring.
integration-test: envtest-assets
	KUBEBUILDER_ASSETS="$$(setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -tags integration -count=1 -timeout 15m ./test/integration/...

# dashboard-check validates the checked-in Grafana dashboard is well-formed
# JSON, so a broken dashboard fails the build like any other defect.
dashboard-check:
	@echo "Validating dashboard JSON..."
	@jq empty dashboards/$(BINARY).json \
		|| { echo "dashboards/$(BINARY).json is not valid JSON"; exit 1; }

# distro-test renders the Kustomize example and the Helm chart.
distro-test:
	@echo "Rendering Kustomize example..."
	kustomize build kubernetes/ > /dev/null
	@echo "Linting Helm chart..."
	helm lint $(CHART)
	@echo "Rendering Helm chart (default values)..."
	helm template $(BINARY) $(CHART) > /dev/null
	@echo "Rendering Helm chart (ServiceMonitor + PodMonitor enabled)..."
	helm template $(BINARY) $(CHART) \
		--set serviceMonitor.enabled=true \
		--set podMonitor.enabled=true > /dev/null
	@echo "Rendering Helm chart (cluster-wide read opt-in)..."
	helm template $(BINARY) $(CHART) --set rbac.clusterReadAll=true > /dev/null

build:
	mkdir -p bin
	go build -ldflags="-X main.serviceVersion=$(VERSION)" -o bin/$(BINARY) ./cmd/$(BINARY)

clean:
	rm -rf bin/

# buildx-builder makes the multi-arch targets work on a stock Docker install
# instead of assuming a suitable builder already exists. Idempotent: it reuses
# the builder if it is already there.
buildx-builder:
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || { \
		echo "Creating buildx builder $(BUILDX_BUILDER) (docker-container driver)..."; \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container >/dev/null; \
	}

docker-build: buildx-builder
	docker buildx build --builder $(BUILDX_BUILDER) \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) .

# docker-verify builds the multi-arch image and checks that each manifest
# actually carries a binary of that architecture. Configuring buildx for two
# platforms is not the same as shipping two correct binaries: a per-arch tag
# holding the wrong ELF still passes every other check.
docker-verify: buildx-builder
	@rm -rf /tmp/$(BINARY)-oci /tmp/$(BINARY)-oci.tar
	docker buildx build --builder $(BUILDX_BUILDER) \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		--output type=oci,dest=/tmp/$(BINARY)-oci.tar .
	@mkdir -p /tmp/$(BINARY)-oci && tar -xf /tmp/$(BINARY)-oci.tar -C /tmp/$(BINARY)-oci
	@./scripts/verify-multiarch.sh /tmp/$(BINARY)-oci $(BINARY)
