BINARY_NAME=bin/manager
IMAGE ?= ghcr.io/henryd24/rollout-ecr-tagger:latest
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.version=$(VERSION)
LOCALBIN ?= $(CURDIR)/bin
SETUP_ENVTEST ?= $(LOCALBIN)/setup-envtest
ENVTEST_K8S_VERSION ?= 1.35.x

# --- Pruebas locales con kind (ver `make kind-help`) ---
KIND_CLUSTER ?= ecr-tagger
KIND_IMAGE ?= rollout-ecr-tagger:kind
KIND_NAMESPACE ?= rollout-ecr-tagger-system
KIND_AWS_CONFIGMAP ?= rollout-ecr-tagger-aws-local
# Todos los targets kind-* fijan el contexto: así nunca actúan sobre otro cluster.
KUBECTL ?= kubectl --context kind-$(KIND_CLUSTER)
KIND_GOARCH ?= $(shell go env GOARCH)
ARGO_ROLLOUTS_VERSION ?= v1.8.0
# true instala el controlador completo de Argo Rollouts (por defecto solo el CRD).
ARGO_FULL ?= false
# Imagen ECR existente que usará el Rollout de prueba (requerida en kind-rollout).
TEST_IMAGE ?=
TEST_NAMESPACE ?= rollout-test
TEST_ROLLOUT ?= test-app

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

# ---------------------------------------------------------------------------
# kind: flujo local rápido, sin push a ECR ni a ningún registro
#   make kind-test TEST_IMAGE=<acct>.dkr.ecr.<region>.amazonaws.com/<repo>:<tag>
# Credenciales AWS: se leen de AWS_PROFILE (o AWS_ACCESS_KEY_ID/...) y se guardan
# en el ConfigMap $(KIND_AWS_CONFIGMAP), que el operador consume vía envFrom.
# SOLO para desarrollo local: un ConfigMap no es un lugar seguro para secretos.
# ---------------------------------------------------------------------------
.PHONY: kind-help
kind-help:
	@echo "Flujo local con kind:"
	@echo "  make kind-test TEST_IMAGE=<ecr-image>  cluster + CRD + build + creds + deploy + rollout de prueba"
	@echo "  make kind-up            crea el cluster kind ($(KIND_CLUSTER)) si no existe"
	@echo "  make kind-argo          instala el CRD de Rollouts (ARGO_FULL=true: controlador completo)"
	@echo "  make kind-build         compila en el host y construye la imagen $(KIND_IMAGE)"
	@echo "  make kind-creds         (re)crea el ConfigMap de credenciales AWS y reinicia el operador"
	@echo "  make kind-deploy        carga la imagen en kind y despliega el operador"
	@echo "  make kind-redeploy      kind-build + kind-deploy (ciclo de desarrollo)"
	@echo "  make kind-rollout TEST_IMAGE=...  crea el Rollout de prueba"
	@echo "  make kind-healthy       simula que el Rollout quedó Healthy (dispara el retag)"
	@echo "  make kind-logs | kind-events | kind-tags | kind-down"
	@echo "Variables: AWS_PROFILE, AWS_REGION, TARGET_ROLE_ARN, KIND_CLUSTER, ARGO_FULL"

.PHONY: kind-check
kind-check:
	@for t in kind kubectl docker; do command -v $$t >/dev/null || { echo "falta '$$t' en el PATH"; exit 1; }; done

.PHONY: kind-up
kind-up: kind-check
	@kind get clusters | grep -qx '$(KIND_CLUSTER)' || kind create cluster --name $(KIND_CLUSTER)
	@kubectl config use-context kind-$(KIND_CLUSTER) >/dev/null

.PHONY: kind-argo
kind-argo: kind-check
ifeq ($(ARGO_FULL),true)
	$(KUBECTL) create namespace argo-rollouts --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/download/$(ARGO_ROLLOUTS_VERSION)/install.yaml
	$(KUBECTL) rollout status deployment/argo-rollouts -n argo-rollouts --timeout=5m
else
	@# El operador solo necesita el CRD; sin controlador no se crean pods (rápido).
	$(KUBECTL) apply --server-side --force-conflicts -f https://raw.githubusercontent.com/argoproj/argo-rollouts/$(ARGO_ROLLOUTS_VERSION)/manifests/crds/rollout-crd.yaml
	$(KUBECTL) wait --for=condition=Established crd/rollouts.argoproj.io --timeout=60s
endif

.PHONY: kind-build
kind-build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(KIND_GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/manager-linux ./cmd/main.go
	docker build -f hack/kind/Dockerfile -t $(KIND_IMAGE) .

.PHONY: kind-creds
kind-creds: kind-check
	@$(KUBECTL) create namespace $(KIND_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f - >/dev/null
	@hack/kind/aws-env.sh | $(KUBECTL) -n $(KIND_NAMESPACE) create configmap $(KIND_AWS_CONFIGMAP) \
		--from-env-file=/dev/stdin --dry-run=client -o yaml | $(KUBECTL) apply -f - >/dev/null
	@echo "ConfigMap $(KIND_NAMESPACE)/$(KIND_AWS_CONFIGMAP) actualizado"
	@if $(KUBECTL) -n $(KIND_NAMESPACE) get deployment rollout-ecr-tagger-controller >/dev/null 2>&1; then \
		$(KUBECTL) -n $(KIND_NAMESPACE) rollout restart deployment/rollout-ecr-tagger-controller && \
		$(KUBECTL) -n $(KIND_NAMESPACE) rollout status deployment/rollout-ecr-tagger-controller --timeout=2m; \
	fi

.PHONY: kind-deploy
kind-deploy: kind-check kind-creds
	kind load docker-image $(KIND_IMAGE) --name $(KIND_CLUSTER)
	sed 's#image: .*rollout-ecr-tagger:.*#image: $(KIND_IMAGE)#' config/operator.yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n $(KIND_NAMESPACE) patch deployment rollout-ecr-tagger-controller --type strategic -p \
		'{"spec":{"template":{"spec":{"containers":[{"name":"manager","envFrom":[{"configMapRef":{"name":"rollout-ecr-tagger-config"}},{"configMapRef":{"name":"$(KIND_AWS_CONFIGMAP)"}}]}]}}}}'
	$(KUBECTL) -n $(KIND_NAMESPACE) rollout restart deployment/rollout-ecr-tagger-controller
	$(KUBECTL) -n $(KIND_NAMESPACE) rollout status deployment/rollout-ecr-tagger-controller --timeout=2m

.PHONY: kind-redeploy
kind-redeploy: kind-build kind-deploy

.PHONY: kind-rollout
kind-rollout: kind-check
	@test -n "$(TEST_IMAGE)" || { echo "Define TEST_IMAGE=<acct>.dkr.ecr.<region>.amazonaws.com/<repo>:<tag> (debe existir en ECR)"; exit 1; }
	$(KUBECTL) create namespace $(TEST_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	sed 's#image: .*amazonaws.com/.*#image: $(TEST_IMAGE)#' examples/rollout-test.yaml | $(KUBECTL) apply -f -

.PHONY: kind-healthy
kind-healthy: kind-check
	@gen=$$(kubectl -n $(TEST_NAMESPACE) get rollout $(TEST_ROLLOUT) -o jsonpath='{.metadata.generation}') && \
		$(KUBECTL) -n $(TEST_NAMESPACE) patch rollout $(TEST_ROLLOUT) --subresource=status --type merge \
			-p "{\"status\":{\"phase\":\"Healthy\",\"observedGeneration\":\"$$gen\"}}"
	@echo "Rollout marcado Healthy; revisa: make kind-logs / make kind-events"

.PHONY: kind-logs
kind-logs: kind-check
	$(KUBECTL) -n $(KIND_NAMESPACE) logs -f deployment/rollout-ecr-tagger-controller

.PHONY: kind-events
kind-events: kind-check
	$(KUBECTL) -n $(TEST_NAMESPACE) describe rollout $(TEST_ROLLOUT)

.PHONY: kind-tags
kind-tags:
	@test -n "$(TEST_IMAGE)" || { echo "Define TEST_IMAGE=..."; exit 1; }
	@repo=$$(echo '$(TEST_IMAGE)' | sed -E 's#^[^/]+/##; s#[:@].*##'); \
		region=$$(echo '$(TEST_IMAGE)' | cut -d. -f4); \
		aws ecr describe-images --repository-name $$repo --region $$region \
		--query 'imageDetails[*].imageTags' --output text | tr '\t' '\n' | sort -u

# Flujo completo: de cero a Rollout de prueba en un solo comando.
.PHONY: kind-test
kind-test: kind-up kind-argo kind-build kind-deploy kind-rollout
	@echo ""
	@echo "Listo. Siguiente: make kind-healthy && make kind-logs   (o make kind-tags TEST_IMAGE=...)"

.PHONY: kind-down
kind-down: kind-check
	kind delete cluster --name $(KIND_CLUSTER)