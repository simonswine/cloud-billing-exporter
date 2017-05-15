ACCOUNT=simonswine
APP_NAME=cloud-billing-exporter

PACKAGE_NAME=github.com/${ACCOUNT}/${APP_NAME}
GO_VERSION=1.8

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

build: version
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-a -tags netgo \
		-o ${BUILD_DIR}/${APP_NAME}-$(GOOS)-$(GOARCH) \
		-ldflags "-X '${PACKAGE_NAME}/vendor/github.com/prometheus/common/version.Version=$(APP_VERSION)' -X '${PACKAGE_NAME}/vendor/github.com/prometheus/common/version.Revision=$(GIT_COMMIT)' -X '${PACKAGE_NAME}/vendor/github.com/prometheus/common/version.Branch=$(GIT_BRANCH)' -X '${PACKAGE_NAME}/vendor/github.com/prometheus/common/version.BuildUser=$(shell id -u -n)@$(shell hostname)' -X '${PACKAGE_NAME}/vendor/github.com/prometheus/common/version.BuildDate=$(shell date --rfc-3339=seconds)'"

version:
	$(eval GIT_STATE := $(shell if test -z "`git status --porcelain 2> /dev/null`"; then echo "clean"; else echo "dirty"; fi))
	$(eval GIT_COMMIT := $(shell git rev-parse HEAD))
	$(eval GIT_BRANCH := $(shell git rev-parse --abbrev-ref HEAD))
	$(eval APP_VERSION ?= $(shell cat VERSION))

image:
	docker build --build-arg VCS_REF=$(GIT_COMMIT) -t $(DOCKER_IMAGE):$(BUILD_TAG) .
	
push: image
	set -e; \
	for tag in $(IMAGE_TAGS); do \
		docker tag  $(DOCKER_IMAGE):$(BUILD_TAG) $(DOCKER_IMAGE):$${tag} ; \
		docker push $(DOCKER_IMAGE):$${tag}; \
	done
