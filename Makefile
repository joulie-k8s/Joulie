REGISTRY ?= registry.cern.ch/mbunino/joulie
TAG ?= latest
NAMESPACE ?= joulie-system

# Image names must follow joulie-<component>, where <component> matches cmd/<component>.
IMAGES ?= joulie-agent joulie-operator

.PHONY: help install uninstall build push build-push rollout build-push-rollout build-push-install print-images

help:
	@echo "Targets:"
	@echo "  make install TAG=<tag>                Apply CRDs/manifests and set image tag"
	@echo "  make uninstall                        Remove manifests and CRD"
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
	kubectl apply -f config/crd/bases/joulie.io_nodepowerprofiles.yaml
	kubectl apply -f deploy/joulie.yaml
	kubectl -n "$(NAMESPACE)" set image daemonset/joulie-agent \
		agent="$(REGISTRY)/joulie-agent:$(TAG)"
	kubectl -n "$(NAMESPACE)" set image deployment/joulie-operator \
		operator="$(REGISTRY)/joulie-operator:$(TAG)"

uninstall:
	kubectl delete -f deploy/joulie.yaml --ignore-not-found=true
	kubectl delete -f config/crd/bases/joulie.io_nodepowerprofiles.yaml --ignore-not-found=true

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
	@echo "Updating image tags in namespace $(NAMESPACE)"
	kubectl -n "$(NAMESPACE)" set image daemonset/joulie-agent \
		agent="$(REGISTRY)/joulie-agent:$(TAG)"
	kubectl -n "$(NAMESPACE)" set image deployment/joulie-operator \
		operator="$(REGISTRY)/joulie-operator:$(TAG)"
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-operator

build-push-rollout: build-push rollout

build-push-install: build-push install
	@echo "Waiting for rollout to complete"
	kubectl -n "$(NAMESPACE)" rollout status daemonset/joulie-agent
	kubectl -n "$(NAMESPACE)" rollout status deployment/joulie-operator
