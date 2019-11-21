ACCOUNT=simonswine
APP_NAME=cloud-billing-exporter

PACKAGE_NAME=github.com/${ACCOUNT}/${APP_NAME}
GO_VERSION=1.13.4

GOOS := linux
GOARCH := amd64

BINDIR ?= $(CURDIR)/bin

DOCKER_IMAGE=${ACCOUNT}/${APP_NAME}

BUILD_DIR=_build
TEST_DIR=_test

CONTAINER_DIR=/go/src/${PACKAGE_NAME}

BUILD_TAG := build
IMAGE_TAGS := canary

# Source URLs / hashes based on OS
UNAME_S := $(shell uname -s)
GOLANGCILINT_VERSION := 1.21.0
ifeq ($(UNAME_S),Linux)
	SHASUM := sha256sum -c
	GOLANGCILINT_URL := https://github.com/golangci/golangci-lint/releases/download/v$(GOLANGCILINT_VERSION)/golangci-lint-$(GOLANGCILINT_VERSION)-linux-amd64.tar.gz
	GOLANGCILINT_HASH := 2c861f8dc56b560474aa27cab0c075991628cc01af3451e27ac82f5d10d5106b
endif
ifeq ($(UNAME_S),Darwin)
	SHASUM := shasum -a 256 -c
	GOLANGCILINT_URL := https://github.com/golangci/golangci-lint/releases/download/v$(GOLANGCILINT_VERSION)/golangci-lint-$(GOLANGCILINT_VERSION)-darwin-amd64.tar.gz
	GOLANGCILINT_HASH := 2b2713ec5007e67883aa501eebb81f22abfab0cf0909134ba90f60a066db3760
endif

# from https://suva.sh/posts/well-documented-makefiles/
.PHONY: help
help:  ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-30s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: version

all: test build

test:
	go test -count 1 ./...

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-a -tags netgo \
		-o ${BUILD_DIR}/${APP_NAME}-$(GOOS)-$(GOARCH) \
		-ldflags "$(shell hack/version-ld-flags.sh)"

image:
	docker build --build-arg VCS_REF=$(shell git rev-parse HEAD) -t $(DOCKER_IMAGE):$(BUILD_TAG) .
	
push: image
	set -e; \
	for tag in $(IMAGE_TAGS); do \
		docker tag  $(DOCKER_IMAGE):$(BUILD_TAG) $(DOCKER_IMAGE):$${tag} ; \
		docker push $(DOCKER_IMAGE):$${tag}; \
	done

.PHONY: lint
lint: $(BINDIR)/golangci-lint ## Run lint through golangci-lint
ifneq ($(CI),)
	$(BINDIR)/golangci-lint run --out-format code-climate  ./... | tee .golangci-lint.json > /dev/null
endif
	$(BINDIR)/golangci-lint run ./...

.PHONY: $(BINDIR)/golangci-lint
$(BINDIR)/golangci-lint: $(BINDIR)/golangci-lint-$(GOLANGCILINT_VERSION)
	@ln -fs golangci-lint-$(GOLANGCILINT_VERSION) $(BINDIR)/golangci-lint

$(BINDIR)/golangci-lint-$(GOLANGCILINT_VERSION):
	mkdir -p $(BINDIR) $(BINDIR)/.golangci-lint
	curl --fail -sL -o $(BINDIR)/.golangci-lint.tar.gz $(GOLANGCILINT_URL)
	echo "$(GOLANGCILINT_HASH)  $(BINDIR)/.golangci-lint.tar.gz" | $(SHASUM)
	tar xvf $(BINDIR)/.golangci-lint.tar.gz -C $(BINDIR)/.golangci-lint
	mv $(BINDIR)/.golangci-lint/*/golangci-lint $(BINDIR)/golangci-lint-$(GOLANGCILINT_VERSION)
	rm -rf $(BINDIR)/.golangci-lint $(BINDIR)/.golangci-lint.tar.gz
