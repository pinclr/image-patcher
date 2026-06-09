# App version is sourced from charts/image-patcher/Chart.yaml so the image tag
# we build always matches the tag the Helm chart will pull.
CHART_DIR    ?= charts/image-patcher
APP_VERSION  := $(shell awk '/^appVersion:/ {gsub(/"/, "", $$2); print $$2}' $(CHART_DIR)/Chart.yaml)

# Controller image identity. The same image can be published to several
# registries at once. IMAGE_REGISTRIES is a space-separated list of
# "<registry>/<namespace>" prefixes; each is combined with IMAGE_REPOSITORY
# and every tag in (IMAGE_TAG + IMAGE_EXTRA_TAGS). For example:
#   make docker-build docker-push \
#     IMAGE_REGISTRIES="ghcr.io/pinclr quay.io/pinclr" IMAGE_EXTRA_TAGS=latest
# A single private registry still works via the back-compat IMAGE_REGISTRY var:
#   make docker-build docker-push IMAGE_REGISTRY=registry.luna.ogpu.cloud
IMAGE_REGISTRY   ?=
IMAGE_REGISTRIES ?= $(strip $(IMAGE_REGISTRY))
IMAGE_REPOSITORY ?= image-patcher-operator
IMAGE_TAG        ?= $(APP_VERSION)
# Additional tags to also build/push alongside IMAGE_TAG (e.g. "latest").
IMAGE_EXTRA_TAGS ?=
ALL_TAGS := $(IMAGE_TAG) $(IMAGE_EXTRA_TAGS)

# Full image references = registries x tags. With no registry set we fall back
# to a bare local tag so `make docker-build` still works for local development.
ifeq ($(strip $(IMAGE_REGISTRIES)),)
IMAGES := $(IMAGE_REPOSITORY):$(IMAGE_TAG)
else
IMAGES := $(foreach r,$(IMAGE_REGISTRIES),$(foreach t,$(ALL_TAGS),$(r)/$(IMAGE_REPOSITORY):$(t)))
endif
# Primary reference used as the local build tag; all others are tagged from it.
IMG := $(firstword $(IMAGES))

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker
PLATFORM ?= linux/amd64

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
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

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= image-patch-operator-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager (tagged for every configured registry).
	$(CONTAINER_TOOL) build --platform $(PLATFORM) -t $(IMG) .
	@for img in $(filter-out $(IMG),$(IMAGES)); do \
		echo "tagging $$img"; \
		$(CONTAINER_TOOL) tag $(IMG) $$img; \
	done

.PHONY: docker-push
docker-push: ## Push docker image(s) to all configured registries. Requires IMAGE_REGISTRIES (or IMAGE_REGISTRY).
	@if [ -z "$(strip $(IMAGE_REGISTRIES))" ]; then \
		echo "ERROR: set IMAGE_REGISTRIES (or IMAGE_REGISTRY) to push, e.g. IMAGE_REGISTRIES=\"ghcr.io/pinclr quay.io/pinclr\"" >&2; exit 1; \
	fi
	@for img in $(IMAGES); do \
		echo "pushing $$img"; \
		$(CONTAINER_TOOL) push $$img; \
	done

##@ Helm

.PHONY: sync-crds
sync-crds: manifests ## Copy generated CRDs from config/crd/bases/ into the chart's crds/ directory.
	rm -f $(CHART_DIR)/crds/*.yaml
	cp config/crd/bases/*.yaml $(CHART_DIR)/crds/

# Example values used to satisfy the chart's values.schema.json during dev.
# Override with VALUES=path/to/your/values.yaml on the command line.
VALUES ?= $(CHART_DIR)/examples/values-ysyb.yaml

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart against $(VALUES).
	$(HELM) lint $(CHART_DIR) -f $(VALUES)

.PHONY: helm-template
helm-template: ## Render the chart with $(VALUES) to /tmp/image-patcher.rendered.yaml for inspection.
	$(HELM) template image-patch $(CHART_DIR) -n image-patch-system -f $(VALUES) > /tmp/image-patcher.rendered.yaml
	@echo "Rendered to /tmp/image-patcher.rendered.yaml"

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: sync-crds ## Install CRDs into the cluster (uses the chart's crds/ directory). For local dev with `make run`.
	$(KUBECTL) apply -f $(CHART_DIR)/crds/

.PHONY: uninstall
uninstall: ## Uninstall CRDs from the cluster. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f $(CHART_DIR)/crds/

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
HELM ?= helm
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.20.0

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.7.2

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
