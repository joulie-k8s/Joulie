REGISTRY ?= registry.cern.ch/mbunino/joulie
TAG ?= latest
NAMESPACE ?= joulie-system
HELM_RELEASE ?= joulie
HELM_CHART ?= charts/joulie
HELM_VALUES ?= values/joulie.yaml
SIM_NAMESPACE ?= joulie-sim-demo
SIM_IMAGE ?= joulie-simulator
SIM_HELM_RELEASE ?= joulie-simulator
SIM_HELM_CHART ?= charts/joulie-simulator
SIM_HELM_VALUES ?= values/joulie-simulator.yaml

# CRDs are generated from api/ by controller-gen and shipped twice: once for
# kubectl apply and once inside the Helm chart. Both copies must be identical.
CONTROLLER_GEN_VERSION ?= v0.16.5
CONTROLLER_GEN ?= $(PWD)/bin/controller-gen
CRD_DIR ?= config/crd/bases
CHART_CRD_DIR ?= charts/joulie/crds

# Image names must follow joulie-<component>, where <component> matches cmd/<component>.
IMAGES ?= joulie-agent joulie-controller-manager joulie-scheduler

.PHONY: help install uninstall build push build-push build-push-all rollout build-push-rollout build-push-install print-images test test-experiments test-all test-examples controller-gen generate manifests verify-manifests kubectl-plugin kubectl-plugin-install kubectl-plugin-push kubectl-plugin-build-push simulator-build simulator-push simulator-build-push simulator-install simulator-uninstall simulator-build-push-deploy simulator-logs docs-serve

help:
	@echo "Targets:"
	@echo "  make install TAG=<tag> [HELM_VALUES=values/joulie.yaml]  Helm install/upgrade"
	@echo "  make uninstall                        Helm uninstall and remove CRD"
	@echo "  make build TAG=<tag>                  Build all images (agent+controller-manager+scheduler)"
	@echo "  make push TAG=<tag>                   Push all images"
	@echo "  make build-push TAG=<tag>             Build and push all images"
	@echo "  make build-push-all TAG=<tag>         Build and push all images + simulator"
	@echo "  make rollout TAG=<tag>                Update and roll out all component images"
	@echo "  make build-push-rollout TAG=<tag>     Build, push, update image, wait rollout"
	@echo "  make build-push-install TAG=<tag>     Build, push, install manifests, wait rollout"
	@echo "  make kubectl-plugin                   Build kubectl-joulie plugin binary"
	@echo "  make kubectl-plugin-install            Build and install kubectl-joulie to /usr/local/bin"
	@echo "  make kubectl-plugin-push TAG=<tag>    Push kubectl-joulie to Harbor (requires oras)"
	@echo "  make kubectl-plugin-build-push TAG=<tag> Build and push kubectl-joulie to Harbor"
	@echo "  make test                             Run unit tests"
	@echo "  make test-envtest                     Run the CRD suite against a real API server (downloads control-plane binaries)"
	@echo "  make test-examples                    Validate example YAML manifests (kubectl dry-run client)"
	@echo "  make ci-local                         Run every check a pull request runs, then print a summary"
	@echo "  make generate                         Regenerate DeepCopy code for api/"
	@echo "  make manifests                        Regenerate CRDs from api/ into config/ and the Helm chart"
	@echo "  make verify-manifests                 Fail if generated code or CRDs are stale (CI)"
	@echo "  make simulator-build TAG=<tag>        Build simulator image"
	@echo "  make simulator-push TAG=<tag>         Push simulator image"
	@echo "  make simulator-build-push TAG=<tag>   Build and push simulator image"
	@echo "  make simulator-install TAG=<tag>      Helm install/upgrade simulator"
	@echo "  make simulator-uninstall              Helm uninstall simulator"
	@echo "  make simulator-build-push-deploy TAG=<tag> Build/push/deploy simulator"
	@echo "  make simulator-logs                   Tail simulator logs"
	@echo "  make docs-serve                       Start Hugo docs server (website/)"
	@echo "  make build IMAGE=<name> TAG=<tag>     Build a single image"
	@echo "  make push IMAGE=<name> TAG=<tag>      Push a single image"

print-images:
	@for img in $(if $(IMAGE),$(IMAGE),$(IMAGES)); do \
		echo "$(REGISTRY)/$$img:$(TAG)"; \
	done

install:
	helm upgrade --install "$(HELM_RELEASE)" "$(HELM_CHART)" \
		-n "$(NAMESPACE)" --create-namespace \
		-f "$(HELM_VALUES)" \
		--set agent.image.repository="$(REGISTRY)/joulie-agent" \
		--set controllerManager.image.repository="$(REGISTRY)/joulie-controller-manager" \
		--set schedulerExtender.image.repository="$(REGISTRY)/joulie-scheduler" \
		--set agent.image.tag="$(TAG)" \
		--set controllerManager.image.tag="$(TAG)" \
		--set schedulerExtender.image.tag="$(TAG)"

uninstall:
	helm uninstall "$(HELM_RELEASE)" -n "$(NAMESPACE)" || true
	kubectl delete crd nodetwins.joulie.io --ignore-not-found=true
	kubectl delete crd nodehardwares.joulie.io --ignore-not-found=true

build:
	@for img in $(if $(IMAGE),$(IMAGE),$(IMAGES)); do \
		component=$${img#joulie-}; \
		echo "Building $(REGISTRY)/$$img:$(TAG)"; \
		docker build --build-arg COMPONENT=$$component -t "$(REGISTRY)/$$img:$(TAG)" -f Dockerfile .; \
	done

push:
	@for img in $(if $(IMAGE),$(IMAGE),$(IMAGES)); do \
		echo "Pushing $(REGISTRY)/$$img:$(TAG)"; \
		docker push "$(REGISTRY)/$$img:$(TAG)"; \
	done

build-push: build push

build-push-all: build-push simulator-build-push

rollout:
	@echo "Rolling out helm release $(HELM_RELEASE) in namespace $(NAMESPACE)"
	helm upgrade --install "$(HELM_RELEASE)" "$(HELM_CHART)" \
		-n "$(NAMESPACE)" --create-namespace \
		-f "$(HELM_VALUES)" \
		--set agent.image.repository="$(REGISTRY)/joulie-agent" \
		--set controllerManager.image.repository="$(REGISTRY)/joulie-controller-manager" \
		--set schedulerExtender.image.repository="$(REGISTRY)/joulie-scheduler" \
		--set agent.image.tag="$(TAG)" \
		--set controllerManager.image.tag="$(TAG)" \
		--set schedulerExtender.image.tag="$(TAG)"
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-controller-manager
	@kubectl -n "$(NAMESPACE)" get deploy/joulie-scheduler-extender >/dev/null 2>&1 && \
		kubectl -n "$(NAMESPACE)" rollout status deploy/joulie-scheduler-extender || true

build-push-rollout: build-push rollout

build-push-install: build-push install
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-controller-manager
	@kubectl -n "$(NAMESPACE)" get deploy/joulie-scheduler-extender >/dev/null 2>&1 && \
		kubectl -n "$(NAMESPACE)" rollout status deploy/joulie-scheduler-extender || true

kubectl-plugin:
	CGO_ENABLED=0 go build -o bin/kubectl-joulie ./cmd/kubectl-joulie

kubectl-plugin-install: kubectl-plugin
	install bin/kubectl-joulie /usr/local/bin/kubectl-joulie

kubectl-plugin-push: kubectl-plugin
	@command -v oras >/dev/null 2>&1 || { echo "oras CLI is required (https://oras.land/)"; exit 1; }
	oras push "$(REGISTRY)/kubectl-joulie:$(TAG)" \
		"bin/kubectl-joulie:application/octet-stream"

kubectl-plugin-build-push: kubectl-plugin kubectl-plugin-push

test:
	go test ./...

# tests/envtest starts a real etcd and kube-apiserver so the generated CRDs
# and the server-side apply field managers are checked by the API server and
# not by a test's idea of it. The `envtest` build tag keeps the control-plane
# binaries out of `make test`. ENVTEST_VERSION tracks the controller-runtime
# minor version in go.mod, and setup-envtest is installed into ./bin the same
# way controller-gen is, so every machine runs the same tool.
.PHONY: test-envtest
ENVTEST_VERSION ?= release-0.19
ENVTEST_K8S_VERSION ?= 1.31.0
ENVTEST_BIN_DIR ?= $(PWD)/bin
SETUP_ENVTEST ?= $(PWD)/bin/setup-envtest

test-envtest:
	@if ! test -x "$(SETUP_ENVTEST)"; then \
		echo "Installing setup-envtest $(ENVTEST_VERSION) into $(PWD)/bin"; \
		GOBIN=$(PWD)/bin go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION); \
	fi
	@assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_BIN_DIR) -p path)"; \
		echo "KUBEBUILDER_ASSETS=$$assets"; \
		KUBEBUILDER_ASSETS="$$assets" go test -tags envtest -count=1 ./tests/envtest/...

# Installs the pinned controller-gen into ./bin unless that exact version is
# already there, so every machine and CI generate the same output.
controller-gen:
	@if ! { test -x "$(CONTROLLER_GEN)" && "$(CONTROLLER_GEN)" --version | grep -q "$(CONTROLLER_GEN_VERSION)"; }; then \
		echo "Installing controller-gen $(CONTROLLER_GEN_VERSION) into $(PWD)/bin"; \
		GOBIN=$(PWD)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION); \
	fi

generate: controller-gen
	$(CONTROLLER_GEN) object paths="./api/..."

# allowDangerousTypes admits float64 fields, which the twin scores and power
# values are.
manifests: controller-gen
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="./api/..." output:crd:artifacts:config=$(CRD_DIR)
	cp $(CRD_DIR)/joulie.io_nodehardwares.yaml $(CRD_DIR)/joulie.io_nodetwins.yaml $(CHART_CRD_DIR)/

verify-manifests: generate manifests
	git diff --exit-code -- api $(CRD_DIR) $(CHART_CRD_DIR)

test-experiments:
	@echo "Running experiment sanity tests..."
	python3 -m pytest experiments/01-cpu-only-benchmark/scripts/test_sweep.py -q --tb=short
	python3 -m pytest experiments/02-heterogeneous-benchmark/scripts/test_sweep.py -q --tb=short
	python3 -m pytest experiments/03-homogeneous-h100-benchmark/scripts/test_sweep.py -q --tb=short
	@echo "All experiment sanity tests passed."

test-all: test test-experiments test-examples

test-examples:
	@set -e; \
	files=$$(find examples -type f -name '*.yaml' | sort); \
	for f in $$files; do \
		if ! grep -q '^apiVersion:' "$$f" || ! grep -q '^kind:' "$$f"; then \
			echo "Skipping patch-like YAML $$f (no apiVersion/kind)"; \
			continue; \
		fi; \
		echo "Validating $$f"; \
		out=$$(kubectl apply --dry-run=client --validate=false -f "$$f" 2>&1 >/dev/null) || rc=$$?; \
		if [ "$${rc:-0}" -ne 0 ]; then \
			if echo "$$out" | grep -Eqi 'unable to recognize|failed to download openapi|couldn.t get current server API group list|connect: connection refused|the server could not find the requested resource'; then \
				echo "Skipping server-dependent validation for $$f (cluster/API discovery unavailable)"; \
				continue; \
			fi; \
			echo "$$out"; \
			exit "$${rc:-1}"; \
		fi; \
	done; \
	echo "All example manifests validated."

.PHONY: ci-local
# ci-local runs, in the same order, every check a pull request runs, so a red
# CI is found before pushing rather than after. Each step runs even when an
# earlier one failed, because the useful answer is the whole list of what is
# broken; the summary at the end names every step and the target exits
# non-zero if any of them failed. The envtest step downloads control plane
# binaries into bin/ the first time, so the first run needs network access.
ci-local:
	@set -u; \
	failed=""; results=""; \
	run() { \
		name="$$1"; shift; \
		printf '\n==> %s\n' "$$name"; \
		if "$$@"; then \
			results="$$results\n  PASS  $$name"; \
		else \
			results="$$results\n  FAIL  $$name"; \
			failed="$$failed $$name"; \
		fi; \
	}; \
	run "make verify-manifests" $(MAKE) --no-print-directory verify-manifests; \
	run "go build ./..." go build ./...; \
	run "go vet ./..." go vet ./...; \
	run "go test ./..." go test ./...; \
	run "make test-envtest" $(MAKE) --no-print-directory test-envtest; \
	run "hack/verify-chart-renders.sh" bash hack/verify-chart-renders.sh; \
	run "helm lint" helm lint charts/joulie charts/joulie-simulator; \
	printf '\n==================== ci-local summary ====================\n'; \
	printf '%b\n' "$$results"; \
	if [ -n "$$failed" ]; then \
		printf '\nci-local: FAIL (failed steps:%s)\n' "$$failed"; \
		exit 1; \
	fi; \
	printf '\nci-local: PASS\n'

simulator-build:
	docker build -f simulator/Dockerfile -t "$(REGISTRY)/$(SIM_IMAGE):$(TAG)" .

simulator-push:
	docker push "$(REGISTRY)/$(SIM_IMAGE):$(TAG)"

simulator-build-push: simulator-build simulator-push

simulator-install:
	helm upgrade --install "$(SIM_HELM_RELEASE)" "$(SIM_HELM_CHART)" \
		-n "$(SIM_NAMESPACE)" --create-namespace \
		$(if $(wildcard $(SIM_HELM_VALUES)),-f "$(SIM_HELM_VALUES)") \
		--set image.repository="$(REGISTRY)/$(SIM_IMAGE)" \
		--set image.tag="$(TAG)"
	kubectl -n "$(SIM_NAMESPACE)" rollout status deploy/joulie-telemetry-sim

simulator-uninstall:
	helm uninstall "$(SIM_HELM_RELEASE)" -n "$(SIM_NAMESPACE)" || true

simulator-build-push-deploy: simulator-build-push simulator-install

simulator-logs:
	kubectl -n "$(SIM_NAMESPACE)" logs -f deploy/joulie-telemetry-sim

docs-serve:
	cd website && hugo server --disableFastRender --ignoreCache --noHTTPCache
