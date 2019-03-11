ACCOUNT=simonswine
APP_NAME=cloud-billing-exporter

PACKAGE_NAME=github.com/${ACCOUNT}/${APP_NAME}
GO_VERSION=1.11.5

GOOS := linux
GOARCH := amd64

DOCKER_IMAGE=${ACCOUNT}/${APP_NAME}

BUILD_DIR=_build
TEST_DIR=_test

CONTAINER_DIR=/go/src/${PACKAGE_NAME}

BUILD_TAG := build
IMAGE_TAGS := canary

PACKAGES=$(shell find . -name "*_test.go" | xargs -n1 dirname | grep -v 'vendor/' | sort -u | xargs -n1 printf "%s.test_pkg ")

.PHONY: version

all: test build

test:
	go test $$(go list ./... | grep -v '/vendor/')

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
