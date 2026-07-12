.PHONY: all build-docker release build test tidy generate generate-api check-gen validate

include ../scripts/blst-portable.mk

VERSION ?= $(shell git describe --always)
BLST_PORTABLE ?= 0
SET_LATEST ?= 0

BUILD_FLAGS := -ldflags "-X main.version=$(VERSION)"

ifeq ($(BLST_PORTABLE),1)
export CGO_CFLAGS := $(CGO_CFLAGS) -D__BLST_PORTABLE__
endif

define DOCKER_BUILD
	@echo "--> building edge-api docker image"
	@echo "    	platform: $(PLATFORM)"
	@echo "    	GOOS: $(GOOS)"
	@echo "    	GOARCH: $(GOARCH)"
	@echo "    	BLST_PORTABLE: $(BLST_PORTABLE)"
	@DOCKER_BUILDKIT=1 docker build \
		--platform $(PLATFORM) \
		--build-arg GOOS=$(GOOS) \
		--build-arg GOARCH=$(GOARCH) \
		--build-arg BLST_PORTABLE=$(BLST_PORTABLE) \
		-f Dockerfile \
		.. \
		-t $(DOCKER_TAG)
endef

all: build-docker

build-docker:
	$(eval PLATFORM=$(DOCKER_PLATFORM))
	$(eval GOOS=$(DOCKER_GOOS))
	$(eval GOARCH=$(DOCKER_GOARCH))
	$(eval DOCKER_TAG=ghcr.io/product-science/edge-api:$(VERSION))
	$(DOCKER_BUILD)
	@if [ "$(SET_LATEST)" = "1" ]; then \
		echo "Setting latest tag..."; \
		docker tag $(DOCKER_TAG) ghcr.io/product-science/edge-api:latest; \
		docker tag $(DOCKER_TAG) edge-api:latest; \
	fi

generate: generate-api

generate-api:
	go generate ./queryapi/...

check-gen: generate-api
	git diff --exit-code queryapi/gen/

build:
	CGO_ENABLED=1 go build $(BUILD_FLAGS) -o build/edge-api ./cmd/edge-api

test:
	go test ./... -count=1

validate:
	@bash ../scripts/validate-edge-api.sh

tidy:
	go mod tidy
