REGISTRY ?= registry.cern.ch/mbunino/joulie
TAG ?= latest
NAMESPACE ?= joulie-system
HELM_RELEASE ?= joulie
HELM_CHART ?= charts/joulie
HELM_VALUES ?= values/joulie.yaml
SIM_NAMESPACE ?= joulie-sim-demo
SIM_IMAGE ?= joulie-simulator

# Image names must follow joulie-<component>, where <component> matches cmd/<component>.
IMAGES ?= joulie-agent joulie-operator

.PHONY: help install uninstall build push build-push build-push-all rollout build-push-rollout build-push-install print-images test test-examples simulator-build simulator-push simulator-build-push simulator-install simulator-build-push-deploy simulator-logs

help:
	@echo "Targets:"
	@echo "  make install TAG=<tag> [HELM_VALUES=values/joulie.yaml]  Helm install/upgrade"
	@echo "  make uninstall                        Helm uninstall and remove CRD"
	@echo "  make build TAG=<tag>                  Build all images"
	@echo "  make push TAG=<tag>                   Push all images"
	@echo "  make build-push TAG=<tag>             Build and push all images"
	@echo "  make build-push-all TAG=<tag>         Build and push agent+operator+simulator"
	@echo "  make rollout TAG=<tag>                Update and roll out agent+operator images"
	@echo "  make build-push-rollout TAG=<tag>     Build, push, update image, wait rollout"
	@echo "  make build-push-install TAG=<tag>     Build, push, install manifests, wait rollout"
	@echo "  make test                             Run unit tests"
	@echo "  make test-examples                    Validate example YAML manifests (kubectl dry-run client)"
	@echo "  make simulator-build TAG=<tag>        Build simulator image"
	@echo "  make simulator-push TAG=<tag>         Push simulator image"
	@echo "  make simulator-build-push TAG=<tag>   Build and push simulator image"
	@echo "  make simulator-install TAG=<tag>      Install simulator using existing image tag"
	@echo "  make simulator-build-push-deploy TAG=<tag> Build/push/deploy simulator"
	@echo "  make simulator-logs                   Tail simulator logs"
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
		--set agent.image.tag="$(TAG)" \
		--set operator.image.tag="$(TAG)"

uninstall:
	helm uninstall "$(HELM_RELEASE)" -n "$(NAMESPACE)" || true
	kubectl delete crd nodepowerprofiles.joulie.io --ignore-not-found=true
	kubectl delete crd telemetryprofiles.joulie.io --ignore-not-found=true

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
		--set agent.image.tag="$(TAG)" \
		--set operator.image.tag="$(TAG)"
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-operator

build-push-rollout: build-push rollout

build-push-install: build-push install
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-operator

test:
	go test ./...

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
	kubectl apply -f simulator/deploy/simulator.yaml || ( \
		echo "Recreating simulator deployment due to immutable selector change"; \
		kubectl -n "$(SIM_NAMESPACE)" delete deploy/joulie-telemetry-sim --ignore-not-found=true; \
		kubectl apply -f simulator/deploy/simulator.yaml \
	)
	kubectl -n "$(SIM_NAMESPACE)" set image deploy/joulie-telemetry-sim \
		simulator="$(REGISTRY)/$(SIM_IMAGE):$(TAG)"
	kubectl -n "$(SIM_NAMESPACE)" rollout status deploy/joulie-telemetry-sim

simulator-build-push-deploy: simulator-build-push simulator-install

simulator-logs:
	kubectl -n "$(SIM_NAMESPACE)" logs -f deploy/joulie-telemetry-sim
