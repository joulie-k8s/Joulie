REGISTRY ?= registry.cern.ch/mbunino/joulie
TAG ?= latest
NAMESPACE ?= joulie-system
HELM_RELEASE ?= joulie
HELM_CHART ?= charts/joulie
HELM_VALUES ?= values/joulie.yaml

# Image names must follow joulie-<component>, where <component> matches cmd/<component>.
IMAGES ?= joulie-agent joulie-operator

.PHONY: help install uninstall build push build-push rollout build-push-rollout build-push-install print-images

help:
	@echo "Targets:"
	@echo "  make install TAG=<tag> [HELM_VALUES=values/joulie.yaml]  Helm install/upgrade"
	@echo "  make uninstall                        Helm uninstall and remove CRD"
	@echo "  make build TAG=<tag>                  Build all images"
	@echo "  make push TAG=<tag>                   Push all images"
	@echo "  make build-push TAG=<tag>             Build and push all images"
	@echo "  make rollout TAG=<tag>                Update and roll out agent+operator images"
	@echo "  make build-push-rollout TAG=<tag>     Build, push, update image, wait rollout"
	@echo "  make build-push-install TAG=<tag>     Build, push, install manifests, wait rollout"
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
