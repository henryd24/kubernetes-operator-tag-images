BINARY_NAME=bin/manager
IMAGE ?= henda24/rollout-ecr-tagger:latest
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.version=$(VERSION)
LOCALBIN ?= $(CURDIR)/bin
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
ENVTEST_K8S_VERSION ?= 1.35.x

.PHONY: build
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/main.go

.PHONY: test
test:
	go test ./...

.PHONY: test-integration
test-integration: $(SETUP_ENVTEST)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use -p path --bin-dir $(LOCALBIN) $(ENVTEST_K8S_VERSION))" \
		go test -race -count=1 ./test/integration/...

$(SETUP_ENVTEST):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23

.PHONY: helm-lint
helm-lint:
	helm lint charts/rollout-ecr-tagger
	helm template test charts/rollout-ecr-tagger > /dev/null

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt-check
fmt-check:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)

.PHONY: run
run:
	go run ./cmd/main.go

.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

.PHONY: docker-push
docker-push:
	docker push $(IMAGE)

.PHONY: deploy
deploy:
	kubectl apply -f config/operator.yaml

.PHONY: undeploy
undeploy:
	kubectl delete -f config/operator.yaml --ignore-not-found=true

.PHONY: test-docker-k3s
test-docker-k3s:
	$(eval TEMP_IMAGE=$(subst :latest,:test,$(IMAGE)))
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(TEMP_IMAGE) --load .
	docker save $(TEMP_IMAGE) -o ./rollout-ecr-tagger.tar
	sudo k3s ctr images import ./rollout-ecr-tagger.tar
	sudo k3s ctr images ls | grep $(TEMP_IMAGE)
	rm -f rollout-ecr-tagger.tar