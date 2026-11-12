SHELL := /bin/sh

REGISTRY ?= bario
TAG ?= latest
PLATFORMS ?= linux/amd64,linux/arm64
BUILDX_BUILDER ?= bario-multiarch
NAMESPACE ?= bario
RELEASE ?= bario
HELM ?= helm
CHART := deploy/bario
SERVICES := auth posts uploads follows ads
GO_PACKAGES := ./pkg/... ./services/auth/... ./services/posts/... ./services/uploads/... ./services/follows/... ./services/ads/...
IMAGE_SETS = \
	--set services.auth.image.repository=$(REGISTRY)/auth \
	--set services.auth.image.tag=$(TAG) \
	--set services.posts.image.repository=$(REGISTRY)/posts \
	--set services.posts.image.tag=$(TAG) \
	--set services.uploads.image.repository=$(REGISTRY)/uploads \
	--set services.uploads.image.tag=$(TAG) \
	--set services.follows.image.repository=$(REGISTRY)/follows \
	--set services.follows.image.tag=$(TAG) \
	--set frontend.image.repository=$(REGISTRY)/frontend \
	--set frontend.image.tag=$(TAG)

.PHONY: all build backend frontend test guard docker-build docker-build-backend docker-build-frontend docker-buildx docker-buildx-builder docker-buildx-backend docker-buildx-frontend helm-install helm-upgrade helm-template helm-lint clean

all: build

## Build all Go services and the frontend bundle.
build: backend frontend

backend:
	cd backend && go build $(GO_PACKAGES)

frontend:
	cd frontend && npm ci && npm run build

## Static checks: vet the Go code and type-check the Vue app.
test:
	cd backend && go vet $(GO_PACKAGES)
	cd frontend && npm run typecheck

## Deterministic, offline secret scan of staged changes.
guard:
	sh scripts/secret_scan.sh --staged

## Build all container images tagged $(REGISTRY)/<name>:$(TAG).
docker-build: docker-build-backend docker-build-frontend

docker-build-backend:
	for svc in $(SERVICES); do \
		docker build -f backend/services/$$svc/Dockerfile -t $(REGISTRY)/$$svc:$(TAG) backend || exit 1; \
	done

docker-build-frontend:
	docker build -t $(REGISTRY)/frontend:$(TAG) frontend

## Create/select the buildx builder used for multi-arch builds.
docker-buildx-builder:
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDX_BUILDER) --driver docker-container
	docker buildx use $(BUILDX_BUILDER)
	docker buildx inspect --bootstrap >/dev/null

## Build and push multi-arch images ($(PLATFORMS)) tagged $(REGISTRY)/<name>:$(TAG).
docker-buildx: docker-buildx-backend docker-buildx-frontend

docker-buildx-backend: docker-buildx-builder
	for svc in $(SERVICES); do \
		docker buildx build --platform $(PLATFORMS) \
			-f backend/services/$$svc/Dockerfile \
			-t $(REGISTRY)/$$svc:$(TAG) --push backend || exit 1; \
	done

docker-buildx-frontend: docker-buildx-builder
	docker buildx build --platform $(PLATFORMS) \
		-t $(REGISTRY)/frontend:$(TAG) --push frontend

## Install or upgrade the chart in one step.
helm-install:
	$(HELM) upgrade --install $(RELEASE) $(CHART) --namespace $(NAMESPACE) --create-namespace $(IMAGE_SETS)

helm-upgrade:
	$(HELM) upgrade $(RELEASE) $(CHART) --namespace $(NAMESPACE) $(IMAGE_SETS)

helm-template:
	$(HELM) template $(RELEASE) $(CHART) --namespace $(NAMESPACE)

helm-lint:
	$(HELM) lint $(CHART)

clean:
	rm -f backend/services/*/auth backend/services/*/posts backend/services/*/uploads backend/services/*/follows backend/services/*/ads
	rm -rf frontend/dist
