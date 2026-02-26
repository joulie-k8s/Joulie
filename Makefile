REGISTRY ?= registry.cern.ch/mbunino/joulie
TAG ?= latest

# Add more image names here as additional Dockerfiles/components are introduced.
IMAGES ?= joulie-agent

.PHONY: help build push build-push print-images

help:
	@echo "Targets:"
	@echo "  make build TAG=<tag>                  Build all images"
	@echo "  make push TAG=<tag>                   Push all images"
	@echo "  make build-push TAG=<tag>             Build and push all images"
	@echo "  make build IMAGE=<name> TAG=<tag>     Build a single image"
	@echo "  make push IMAGE=<name> TAG=<tag>      Push a single image"

print-images:
	@for img in $(if $(IMAGE),$(IMAGE),$(IMAGES)); do \
		echo "$(REGISTRY)/$$img:$(TAG)"; \
	done

build:
	@for img in $(if $(IMAGE),$(IMAGE),$(IMAGES)); do \
		echo "Building $(REGISTRY)/$$img:$(TAG)"; \
		docker build -t "$(REGISTRY)/$$img:$(TAG)" -f Dockerfile .; \
	done

push:
	@for img in $(if $(IMAGE),$(IMAGE),$(IMAGES)); do \
		echo "Pushing $(REGISTRY)/$$img:$(TAG)"; \
		docker push "$(REGISTRY)/$$img:$(TAG)"; \
	done

build-push: build push
