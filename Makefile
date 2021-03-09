# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

PKG = sigs.k8s.io/azuredisk-csi-driver
GIT_COMMIT ?= $(shell git rev-parse HEAD)
REGISTRY ?= andyzhangx
REGISTRY_NAME ?= $(shell echo $(REGISTRY) | sed "s/.azurecr.io//g")
DRIVER_NAME = disk.csi.azure.com
IMAGE_NAME ?= azuredisk-csi
SCHEDULER_EXTENDER_IMAGE_NAME ?= azdiskschedulerextender-csi
ifndef BUILD_V2
PLUGIN_NAME = azurediskplugin
IMAGE_VERSION ?= v1.2.0
CHART_VERSION ?= latest
else
PLUGIN_NAME = azurediskpluginv2
IMAGE_VERSION ?= v2.0.0-alpha.1
CHART_VERSION ?= v2.0.0-alpha.1
GOTAGS += -tags azurediskv2
endif
CLOUD ?= AzurePublicCloud
# Use a custom version for E2E tests if we are testing in CI
ifdef CI
ifndef PUBLISH
override IMAGE_VERSION := $(IMAGE_VERSION)-$(GIT_COMMIT)
endif
endif
IMAGE_TAG ?= $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_VERSION)
IMAGE_TAG_LATEST = $(REGISTRY)/$(IMAGE_NAME):latest
AZ_DISK_SCHEDULER_EXTENDER_IMAGE_TAG ?= $(REGISTRY)/$(SCHEDULER_EXTENDER_IMAGE_NAME):$(IMAGE_VERSION)
AZ_DISK_SCHEDULER_EXTENDER_IMAGE_TAG_LATEST = $(REGISTRY)/$(SCHEDULER_EXTENDER_IMAGE_NAME):latest
REV = $(shell git describe --long --tags --dirty)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
TOPOLOGY_KEY = topology.$(DRIVER_NAME)/zone
ENABLE_TOPOLOGY ?= false
SCHEDULER_EXTENDER_LDFLAGS ?= "-X ${PKG}/pkg/azuredisk.schedulerVersion=${IMAGE_VERSION} -X ${PKG}/pkg/azuredisk.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/azuredisk.buildDate=${BUILD_DATE} -X ${PKG}/pkg/azuredisk.DriverName=${DRIVER_NAME} -extldflags "-static""
LDFLAGS ?= "-X ${PKG}/pkg/azuredisk.driverVersion=${IMAGE_VERSION} -X ${PKG}/pkg/azuredisk.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/azuredisk.buildDate=${BUILD_DATE} -X ${PKG}/pkg/azuredisk.DriverName=${DRIVER_NAME} -X ${PKG}/pkg/azuredisk.topologyKey=${TOPOLOGY_KEY} -extldflags "-static"" ${GOTAGS}
E2E_HELM_OPTIONS ?= --set image.azuredisk.repository=$(REGISTRY)/$(IMAGE_NAME) --set image.azuredisk.tag=$(IMAGE_VERSION) --set image.azuredisk.pullPolicy=Always ${ADDITIONAL_E2E_HELM_OPTIONS}
GINKGO_FLAGS = -ginkgo.v
ifeq ($(ENABLE_TOPOLOGY), true)
GINKGO_FLAGS += -ginkgo.focus="\[multi-az\]"
else
GINKGO_FLAGS += -ginkgo.focus="\[single-az\]"
endif
GOPATH ?= $(shell go env GOPATH)
GOBIN ?= $(GOPATH)/bin
GO111MODULE = on
DOCKER_CLI_EXPERIMENTAL = enabled
export GOPATH GOBIN GO111MODULE DOCKER_CLI_EXPERIMENTAL

# Generate all combination of all OS, ARCH, and OSVERSIONS for iteration
ALL_OS = linux windows
ALL_ARCH.linux = amd64
ALL_OS_ARCH.linux = $(foreach arch, ${ALL_ARCH.linux}, linux-$(arch))
ALL_ARCH.windows = amd64
ALL_OSVERSIONS.windows := 1809 1903 1909 2004
ALL_OS_ARCH.windows = $(foreach arch, $(ALL_ARCH.windows), $(foreach osversion, ${ALL_OSVERSIONS.windows}, windows-${osversion}-${arch}))
ALL_OS_ARCH = $(foreach os, $(ALL_OS), ${ALL_OS_ARCH.${os}})

# The current context of image building
# The architecture of the image
ARCH ?= amd64
# OS Version for the Windows images: 1809, 1903, 1909, 2004
OSVERSION ?= 1809
# Output type of docker buildx build
OUTPUT_TYPE ?= registry

.PHONY: all
all: azuredisk

.PHONY: verify
verify: unit-test
	hack/verify-all.sh
	go vet ./pkg/...

.PHONY: unit-test
unit-test: unit-test-v1 unit-test-v2

.PHONY: unit-test-v1
unit-test-v1:
	go test -v -cover ./pkg/... ./test/utils/credentials

.PHONY: unit-test-v2
unit-test-v2:
	go test -v -cover -tags azurediskv2 ./pkg/azuredisk --temp-use-driver-v2

.PHONY: sanity-test
sanity-test: azuredisk
	go test -v -timeout=30m ./test/sanity

.PHONY: sanity-test-v2
sanity-test-v2: azuredisk-v2
	go test -v -timeout=30m ./test/sanity --temp-use-driver-v2

.PHONY: integration-test
integration-test: azuredisk
	go test -v -timeout=30m ./test/integration

.PHONY: integration-test-v2
integration-test-v2: azuredisk-v2
	go test -v -timeout=30m ./test/integration --temp-use-driver-v2

.PHONY: e2e-test
e2e-test:
	go test -v -timeout=0 ./test/e2e ${GINKGO_FLAGS}

.PHONY: e2e-bootstrap
e2e-bootstrap: install-helm
	docker pull $(IMAGE_TAG) || make container-all push-manifest
ifdef TEST_WINDOWS
	helm install azuredisk-csi-driver charts/${CHART_VERSION}/azuredisk-csi-driver --namespace kube-system --wait --timeout=15m -v=5 --debug \
		${E2E_HELM_OPTIONS} \
		--set windows.enabled=true \
		--set linux.enabled=false \
		--set controller.runOnMaster=true \
		--set controller.replicas=1 \
		--set controller.logLevel=6 \
		--set node.logLevel=6 \
		--set cloud=$(CLOUD)
else
	helm upgrade --install azuredisk-csi-driver charts/${CHART_VERSION}/azuredisk-csi-driver --namespace kube-system --wait --timeout=15m -v=5 --debug \
		${E2E_HELM_OPTIONS} \
		--set snapshot.enabled=true \
		--set cloud=$(CLOUD)
endif

.PHONY: install-helm
install-helm:
	curl https://raw.githubusercontent.com/helm/helm/master/scripts/get-helm-3 | bash

.PHONY: e2e-teardown
e2e-teardown:
	helm delete azuredisk-csi-driver --namespace kube-system

.PHONY: azuredisk
azuredisk:
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags ${LDFLAGS} -mod vendor -o _output/${PLUGIN_NAME} ./pkg/azurediskplugin

.PHONY: azuredisk-v2
azuredisk-v2:
	BUILD_V2=1 $(MAKE) azuredisk

.PHONY: azuredisk-windows
azuredisk-windows:
	CGO_ENABLED=0 GOOS=windows go build -a -ldflags ${LDFLAGS} -mod vendor -o _output/${PLUGIN_NAME}.exe ./pkg/azurediskplugin

.PHONY: azuredisk-windows-v2
azuredisk-windows-v2:
	BUILD_V2=1 $(MAKE) azuredisk

.PHONY: azuredisk-darwin
azuredisk-darwin:
	CGO_ENABLED=0 GOOS=darwin go build -a -ldflags ${LDFLAGS} -mod vendor -o _output/azurediskplugin ./pkg/azurediskplugin

.PHONY: azdiskschedulerextender
azdiskschedulerextender:
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags ${SCHEDULER_EXTENDER_LDFLAGS} -mod vendor -o _output/azdiskschedulerextender ./pkg/azdiskschedulerextender

.PHONY: azdiskschedulerextender-windows
azdiskschedulerextender-windows:
	CGO_ENABLED=0 GOOS=windows go build -a -ldflags ${SCHEDULER_EXTENDER_LDFLAGS} -mod vendor -o _output/azdiskschedulerextender.exe ./pkg/azdiskschedulerextender

.PHONY: azdiskschedulerextender-darwin
azdiskschedulerextender-darwin:
	CGO_ENABLED=0 GOOS=darwin go build -a -ldflags ${SCHEDULER_EXTENDER_LDFLAGS} -mod vendor -o _output/azdiskschedulerextender ./pkg/azdiskschedulerextender

.PHONY: container
container: azuredisk
	docker build --no-cache -t $(IMAGE_TAG) --build-arg PLUGIN_NAME=${PLUGIN_NAME} -f ./pkg/azurediskplugin/dev.Dockerfile .

.PHONY: container-linux
container-linux:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="linux/$(ARCH)" --build-arg PLUGIN_NAME=${PLUGIN_NAME} \
		-t $(IMAGE_TAG)-linux-$(ARCH) -f ./pkg/azurediskplugin/Dockerfile .

.PHONY: container-windows
container-windows:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="windows/$(ARCH)" --build-arg PLUGIN_NAME=${PLUGIN_NAME} \
		 -t $(IMAGE_TAG)-windows-$(OSVERSION)-$(ARCH) --build-arg OSVERSION=$(OSVERSION) -f ./pkg/azurediskplugin/Windows.Dockerfile .

.PHONY: azdiskschedulerextender-container
azdiskschedulerextender-container: azdiskschedulerextender
	docker build --no-cache -t $(AZ_DISK_SCHEDULER_EXTENDER_IMAGE_TAG) -f ./pkg/azdiskschedulerextender/dev.Dockerfile .

.PHONY: azdiskschedulerextender-container-linux
azdiskschedulerextender-container-linux:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="linux/$(ARCH)" \
		-t $(AZ_DISK_SCHEDULER_EXTENDER_IMAGE_TAG)-linux-$(ARCH) -f ./pkg/azdiskschedulerextender/Dockerfile .

.PHONY: azdiskschedulerextender-container-windows
azdiskschedulerextender-container-windows:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="windows/$(ARCH)" \
		 -t $(AZ_DISK_SCHEDULER_EXTENDER_IMAGE_TAG)-windows-$(OSVERSION)-$(ARCH) --build-arg OSVERSION=$(OSVERSION) -f ./pkg/azdiskschedulerextender/Windows.Dockerfile .

.PHONY: container-all
container-all: azuredisk azuredisk-windows
	docker buildx rm container-builder || true
	docker buildx create --use --name=container-builder
ifeq ($(CLOUD), AzureStackCloud)
	docker run --privileged --name buildx_buildkit_container-builder0 -d --mount type=bind,src=/etc/ssl/certs,dst=/etc/ssl/certs moby/buildkit:latest || true
endif
	$(MAKE) container-linux
	for osversion in $(ALL_OSVERSIONS.windows); do \
		OSVERSION=$${osversion} $(MAKE) container-windows; \
	done

.PHONY: push-manifest
push-manifest:
	# TODO: Add publish commands for azdiskschedulerextender
	docker manifest create --amend $(IMAGE_TAG) $(foreach osarch, $(ALL_OS_ARCH), $(IMAGE_TAG)-${osarch})
	# add "os.version" field to windows images (based on https://github.com/kubernetes/kubernetes/blob/master/build/pause/Makefile)
	set -x; \
	registry_prefix=$(shell (echo ${REGISTRY} | grep -Eq ".*[\/\.].*") && echo "" || echo "docker.io/"); \
	manifest_image_folder=`echo "$${registry_prefix}${IMAGE_TAG}" | sed "s|/|_|g" | sed "s/:/-/"`; \
	for arch in $(ALL_ARCH.windows); do \
		for osversion in $(ALL_OSVERSIONS.windows); do \
			BASEIMAGE=mcr.microsoft.com/windows/nanoserver:$${osversion}; \
			full_version=`docker manifest inspect $${BASEIMAGE} | jq -r '.manifests[0].platform["os.version"]'`; \
			sed -i -r "s/(\"os\"\:\"windows\")/\0,\"os.version\":\"$${full_version}\"/" "${HOME}/.docker/manifests/$${manifest_image_folder}/$${manifest_image_folder}-windows-$${osversion}-$${arch}"; \
		done; \
	done
	docker manifest push --purge $(IMAGE_TAG)
	docker manifest inspect $(IMAGE_TAG)
ifdef PUBLISH
	docker manifest create $(IMAGE_TAG_LATEST) $(foreach osarch, $(ALL_OS_ARCH), $(IMAGE_TAG)-${osarch})
	docker manifest inspect $(IMAGE_TAG_LATEST)
endif

.PHONY: push-latest
push-latest:
ifdef CI
	docker manifest push --purge $(IMAGE_TAG_LATEST)
else
	docker push $(IMAGE_TAG_LATEST)
endif

.PHONY: push-latest-azdiskschedulerextender
push-latest-azdiskschedulerextender:
	# TODO: Add publish commands for azdiskschedulerextender in CI environment
	docker push $(AZ_DISK_SCHEDULER_EXTENDER_IMAGE_TAG)

.PHONY: clean
clean:
	go clean -r -x
	-rm -rf _output

.PHONY: create-metrics-svc
create-metrics-svc:
	kubectl create -f deploy/example/metrics/csi-azuredisk-controller-svc.yaml

.PHONY: delete-metrics-svc
delete-metrics-svc:
	kubectl delete -f deploy/example/metrics/csi-azuredisk-controller-svc.yaml --ignore-not-found

