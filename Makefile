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
