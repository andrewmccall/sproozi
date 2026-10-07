# Image URL to use all building/pushing image targets
IMG ?= controller:latest
CODEX_IMAGE ?=
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

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
DEPLOY_KUSTOMIZATION ?= config/install

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
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-e2e
test-e2e: ## Create an isolated Kind cluster with enforcing CNI and run the e2e tests.
	@KIND_NODE_IMAGE='$(KIND_NODE_IMAGE)' bash hack/verify/kind.sh

.PHONY: test-e2e-existing
test-e2e-existing: ## Run e2e tests against the caller-owned, already-created Kind cluster.
	@KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: verify-assertions
verify-assertions: ## Validate the complete, individually named offline assertion manifest and emit a redacted declaration report.
	python3 hack/verify/assertions.py --report ".local/verification/manifest-$$(date -u +%Y%m%dT%H%M%SZ).json"

.PHONY: verify-fast
verify-fast: verify-assertions ## Run the non-network unit, race, and vet gates without changing source artifacts.
	cache_dir="$$(mktemp -d)" && GOCACHE="$$cache_dir" go test -race ./api/v1alpha1 ./internal/agentcontract ./internal/execution ./internal/kubernetes ./internal/packages ./internal/policy && GOCACHE="$$cache_dir" go vet ./internal/agentcontract ./internal/execution ./internal/kubernetes ./internal/packages ./internal/policy

.PHONY: verify-api
verify-api: ## Run API, controller, Kubernetes, and envtest checks without regenerating source artifacts.
	cache_dir="$$(mktemp -d)" && GOCACHE="$$cache_dir" go test ./api/v1alpha1 ./internal/controller ./internal/kubernetes ./test/envtest

.PHONY: verify-mcp
verify-mcp: ## Exercise native and configured MCP tools through authenticated TLS CONNECT, including policy and lifecycle denials.
	go test -race ./internal/endpoints/kubernetes ./internal/endpoints/mcp ./internal/mcppolicy ./internal/harness ./cmd/gateway -count=1
	python3 hack/verify/mcp-live-test.py

.PHONY: verify-mcp-client
verify-mcp-client: ## Verify generated run-local configuration with the installed stock Codex CLI, without a model request.
	python3 hack/verify/mcp-client.py

.PHONY: verify-mcp-kind
verify-mcp-kind: ## Prove configured providers with stock Codex in owned Kind; requires pinned images, CNI and a single-owner model session.
	bash hack/verify/mcp-kind.sh

.PHONY: verify-protocol
verify-protocol: protocol-tools ## Run pinned-client offline fixtures and protocol package tests without silently accepting zero tests.
	export PATH="$(abspath $(LOCALBIN)/protocol):$$PATH"; cache_dir="$$(mktemp -d)" && SPROOZI_PROTOCOL_FIXTURE=1 GOCACHE="$$cache_dir" hack/verify/go-test-required.sh ./internal/endpoints/github '^(TestPinnedGitReceivePackProtocolFixture|TestPinnedGHPRCreateStartsWithGraphQL|TestPinnedGHAPICreatesPRUsingBoundedREST|TestInspectReceivePack.*|TestParsePushCommands.*|TestGraphQL.*|TestPackage.*)$$' && GOCACHE="$$cache_dir" go test ./internal/endpoints/packages ./internal/budget ./internal/endpoints/destination ./internal/gateway ./internal/endpoints/model ./internal/proxytransport -count=1

.PHONY: protocol-tools
protocol-tools: $(LOCALBIN) ## Install the checksum-pinned gh client used by the protocol fixtures.
	bash hack/verify/install-gh.sh "$(LOCALBIN)/protocol"

.PHONY: demo-check
demo-check: demo-setup ## Check local prerequisites for the disposable manual AgentRun demo.
	@case "$(IMG)" in *:latest|latest) echo "Set IMG to a non-latest local tag, for example controller:sproozi-demo" >&2; exit 1;; esac
	@CONTAINER_TOOL='$(CONTAINER_TOOL)' KIND='$(KIND)' KUBECTL='$(KUBECTL)' bash hack/demo/check.sh

.PHONY: demo-setup
demo-setup: ## Configure Kind networking and a local pinned stock Codex image without prompts.
	@CONTAINER_TOOL='$(CONTAINER_TOOL)' bash hack/demo/setup.sh

.PHONY: demo-chatgpt-login
demo-chatgpt-login: ## Authorize ChatGPT plan usage locally; no credentials enter the sandbox.
	@bash hack/demo/chatgpt-login.sh

.PHONY: demo
demo: demo-check ## Build/load, deploy, and start the manual AgentRun demo on Kind.
	@set -e; \
	env_file="$${SPROOZI_DEMO_ENV_FILE:-$(CURDIR)/.local/demo.env}"; \
	if [[ -f "$$env_file" ]]; then set -a; source "$$env_file"; set +a; fi; \
	cluster='$(KIND_CLUSTER)'; \
	mkdir -p '$(CURDIR)/.local/demo'; \
	kubeconfig='$(CURDIR)/.local/demo/'"$$cluster"'.kubeconfig'; \
	case "$$($(KIND) get clusters 2>/dev/null)" in *"$$cluster"*) echo "Cluster $$cluster already exists; choose a new KIND_CLUSTER or delete the disposable cluster" >&2; exit 1 ;; esac; \
	$(KIND) create cluster --name "$$cluster" --config hack/kind-cluster.yaml --image '$(KIND_NODE_IMAGE)' --kubeconfig "$$kubeconfig"; \
	chmod 600 "$$kubeconfig"; \
	export KUBECONFIG="$$kubeconfig"; \
	$(KUBECTL) apply --filename "$$SPROOZI_CNI_MANIFEST"; \
	$(KUBECTL) wait --namespace kube-system --for=create pods --selector "$$SPROOZI_CNI_SELECTOR" --timeout="$${SPROOZI_CNI_TIMEOUT:-180s}"; \
	$(KUBECTL) wait --namespace kube-system --for=condition=Ready pods --selector "$$SPROOZI_CNI_SELECTOR" --timeout="$${SPROOZI_CNI_TIMEOUT:-180s}"; \
	$(MAKE) docker-build IMG='$(IMG)'; \
	$(CONTAINER_TOOL) pull "$$CODEX_IMAGE"; \
	$(KIND) load docker-image '$(IMG)' --name "$$cluster"; \
	CONTAINER_TOOL='$(CONTAINER_TOOL)' KIND='$(KIND)' bash hack/demo/load-image.sh "$$cluster"; \
	$(MAKE) install; \
	$(KUBECTL) apply -f examples/sre-demo/namespaces.yaml; \
	CONTAINER_TOOL='$(CONTAINER_TOOL)' KIND='$(KIND)' KUBECTL='$(KUBECTL)' bash hack/demo/prepare.sh; \
	$(MAKE) deploy IMG='$(IMG)'; \
	$(KUBECTL) rollout status deployment/sproozi-controller-manager -n sproozi-system --timeout=180s; \
	$(KUBECTL) rollout status deployment/sproozi-gateway -n sproozi-system --timeout=180s; \
	python3 hack/demo/render.py --codex-image "$$CODEX_IMAGE" --repository "$$SPROOZI_DEMO_REPOSITORY" --model-auth "$${MODEL_AUTH_MODE:-api_key}" | $(KUBECTL) apply -f -; \
	SPROOZI_LIVE_ACCEPT=1 SPROOZI_DEMO_REPO="$$SPROOZI_DEMO_REPOSITORY" SPROOZI_KUBECONFIG="$$kubeconfig" bash hack/verify/live.sh; \
	echo "Export the isolated context with: export KUBECONFIG=$$kubeconfig"

.PHONY: verify-kind
verify-kind: ## Run the real Kind acceptance gate with an explicit kubeconfig and enforcing CNI.
	KIND_NODE_IMAGE='$(KIND_NODE_IMAGE)' bash hack/verify/kind.sh

.PHONY: verify-live
verify-live: ## Run opt-in live acceptance; creates a real PR in SPROOZI_DEMO_REPO.
	bash hack/verify/live.sh

.PHONY: verify-release
verify-release: ## Run every required verification gate without publishing or pushing.
	bash hack/verify/release.sh

.PHONY: verify-docs
verify-docs: ## Check local Markdown links and private-plan references.
	python3 hack/verify/docs.py

.PHONY: verify-public-tree
verify-public-tree: ## Check that private local material is absent from the tracked tree.
	bash hack/verify/public-tree.sh

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name sproozi-builder
	$(CONTAINER_TOOL) buildx use sproozi-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm sproozi-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/install > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply --server-side --field-manager=sproozi-installer -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build "$(DEPLOY_KUSTOMIZATION)" | "$(KUBECTL)" apply --server-side --field-manager=sproozi-installer -f -
	"$(KUBECTL)" apply -k config/agent-rbac

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/install | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -
	"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -k config/agent-rbac

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
# Keep the e2e tool and node image immutable across reruns.
KIND_VERSION ?= v0.29.0
KIND_NODE_IMAGE ?= kindest/node:v1.33.1@sha256:050072256b9a903bd914c0b2866828150cb229cea0efe5892e2b644d5dd3b34f
# Official Kind v0.29.0 release checksums (Linux architectures used by CI).
KIND_LINUX_AMD64_SHA256 ?= c72eda46430f065fb45c5f70e7c957cc9209402ef309294821978677c8fb3284
KIND_LINUX_ARM64_SHA256 ?= 03d45095dbd9cc1689f179a3e5e5da24b77c2d1b257d7645abf1b4174bebcf2a
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

# ENVTEST_VERSION is maintained independently because setup-envtest is a
# standalone module with its own release cadence.
ENVTEST_VERSION ?= v0.24.1

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.12.2
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): | $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): | $(LOCALBIN)
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
$(ENVTEST): | $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): | $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

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
