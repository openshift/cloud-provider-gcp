# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif


VERSION     ?= $(shell git describe --always --abbrev=7)
MUTABLE_TAG ?= latest
IMAGE       ?= gcp-cloud-controller-manager
BUILD_IMAGE ?= registry.ci.openshift.org/openshift/release:golang-1.20

ifeq ($(shell command -v podman > /dev/null 2>&1 ; echo $$? ), 0)
	ENGINE=podman
else ifeq ($(shell command -v docker > /dev/null 2>&1 ; echo $$? ), 0)
	ENGINE=docker
endif

USE_DOCKER ?= 0
ifeq ($(USE_DOCKER), 1)
	ENGINE=docker
endif


.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: vendor
vendor: ## Ensure the vendor directory is up to date.
	go mod tidy
	go mod vendor
	go mod verify

.PHONY: lint
lint: ## Run golangci-lint over the codebase.
	$(call ensure-home, ${GOLANGCI_LINT} run ./... --timeout 5m)
	./openshift-hack/scripts/verify-log-keys.sh

.PHONY: test
test: fmt vet unit ## Run tests.

.PHONY: verify-%
verify-%: ## Ensure no diff after running some other target
	make $*
	./openshift-hack/scripts/verify-diff.sh

##@ Build

.PHONY: build
build:fmt vet ## Build manager binary.
	go build -o bin/gcp-cloud-controller-manager ./cmd/cloud-controller-manager


.PHONY: images
images: ## Create images
	$(ENGINE) build -t "$(IMAGE):$(VERSION)" -t "$(IMAGE):$(MUTABLE_TAG)" ./

.PHONY: push
push: ## Push images
	$(ENGINE) push "$(IMAGE):$(VERSION)"
	$(ENGINE) push "$(IMAGE):$(MUTABLE_TAG)"
