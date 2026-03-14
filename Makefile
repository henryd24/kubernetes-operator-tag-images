BINARY_NAME=bin/manager
IMAGE ?= henda24/rollout-ecr-tagger:latest

.PHONY: build
build:
	go build -o $(BINARY_NAME) ./cmd/main.go

.PHONY: test
test:
	go test ./...

.PHONY: run
run:
	go run ./cmd/main.go

.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE) .

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
	docker buildx build --platform linux/amd64 -t $(TEMP_IMAGE) --load .
	docker save $(TEMP_IMAGE) -o ./rollout-ecr-tagger.tar
	sudo k3s ctr images import ./rollout-ecr-tagger.tar
	sudo k3s ctr images ls | grep $(TEMP_IMAGE)
	rm -f rollout-ecr-tagger.tar