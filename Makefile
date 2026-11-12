SHELL := /bin/sh

REGISTRY ?= chirp
TAG ?= latest
NAMESPACE ?= chirp
RELEASE ?= chirp
HELM ?= helm
CHART := deploy/chirp
SERVICES := auth posts uploads follows
GO_PACKAGES := ./pkg/... ./services/auth/... ./services/posts/... ./services/uploads/... ./services/follows/...
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

.PHONY: all build backend frontend test docker-build docker-build-backend docker-build-frontend helm-install helm-upgrade helm-template helm-lint clean

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

## Build all container images tagged $(REGISTRY)/<name>:$(TAG).
docker-build: docker-build-backend docker-build-frontend

docker-build-backend:
	for svc in $(SERVICES); do \
		docker build -f backend/services/$$svc/Dockerfile -t $(REGISTRY)/$$svc:$(TAG) backend || exit 1; \
	done

docker-build-frontend:
	docker build -t $(REGISTRY)/frontend:$(TAG) frontend

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
	rm -rf frontend/dist
