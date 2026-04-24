# Run all tests (Go + Frontend)
test: test-go test-frontend

test-go:
	go test ./...

test-frontend:
	yarn test

# Run all tests with coverage
coverage: coverage-go coverage-frontend

coverage-go:
	@mkdir -p coverage-go
	go test -coverprofile=coverage-go/coverage.out ./pkg/api/... ./pkg/database/...
	go tool cover -func=coverage-go/coverage.out
	go tool cover -html=coverage-go/coverage.out -o coverage-go/coverage.html
	@echo "HTML report: coverage-go/coverage.html"

coverage-frontend:
	yarn test -- --coverage

# Image URL to use all building/pushing image targets
REGISTRY ?= quay.io
REPOSITORY ?= $(REGISTRY)/eformat/gpu-booking-plugin

IMG := $(REPOSITORY):latest
PODMAN_ARGS ?=

# clean compile
compile:
	yarn run build

# Podman Login
podman-login:
	@podman login -u $(DOCKER_USER) -p $(DOCKER_PASSWORD) $(REGISTRY)

# Build the oci image no compile
podman-build-nocompile:
	podman build $(PODMAN_ARGS) . -t ${IMG} -f Containerfile

# Build the oci image
podman-build: compile
	podman build $(PODMAN_ARGS) . -t ${IMG} -f Containerfile

# Push the oci image
podman-push: podman-build
	podman push ${IMG}

# Push the oci image
podman-push-nocompile: podman-build-nocompile
	podman push ${IMG}

# Just Push the oci image
podman-push-nobuild:
	podman push ${IMG}
