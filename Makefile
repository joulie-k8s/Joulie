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

# Image names must follow joulie-<component>, where <component> matches cmd/<component>.
IMAGES ?= joulie-agent joulie-operator joulie-scheduler

.PHONY: help install uninstall build push build-push build-push-all rollout build-push-rollout build-push-install print-images test test-experiments test-all test-examples kubectl-plugin kubectl-plugin-install kubectl-plugin-push kubectl-plugin-build-push simulator-build simulator-push simulator-build-push simulator-install simulator-uninstall simulator-build-push-deploy simulator-logs docs-serve

help:
	@echo "Targets:"
	@echo "  make install TAG=<tag> [HELM_VALUES=values/joulie.yaml]  Helm install/upgrade"
	@echo "  make uninstall                        Helm uninstall and remove CRD"
	@echo "  make build TAG=<tag>                  Build all images (agent+operator+scheduler)"
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
	@echo "  make test-examples                    Validate example YAML manifests (kubectl dry-run client)"
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
		--set operator.image.repository="$(REGISTRY)/joulie-operator" \
		--set schedulerExtender.image.repository="$(REGISTRY)/joulie-scheduler" \
		--set agent.image.tag="$(TAG)" \
		--set operator.image.tag="$(TAG)" \
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
		--set operator.image.repository="$(REGISTRY)/joulie-operator" \
		--set schedulerExtender.image.repository="$(REGISTRY)/joulie-scheduler" \
		--set agent.image.tag="$(TAG)" \
		--set operator.image.tag="$(TAG)" \
		--set schedulerExtender.image.tag="$(TAG)"
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-operator
	@kubectl -n "$(NAMESPACE)" get deploy/joulie-scheduler-extender >/dev/null 2>&1 && \
		kubectl -n "$(NAMESPACE)" rollout status deploy/joulie-scheduler-extender || true

build-push-rollout: build-push rollout

build-push-install: build-push install
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-operator
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
