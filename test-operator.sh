#!/bin/bash

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

AWS_ACCOUNT_ID=""
AWS_REGION="us-east-1"
AWS_ROLE_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:role/rollout-ecr-tagger-controller-role"


# Default values
NAMESPACE="rollout-test"
OPERATOR_NAMESPACE="rollout-ecr-tagger-system"
ACCOUNT_ID="${AWS_ACCOUNT_ID}"
REGION="${AWS_REGION:-us-east-1}"
ROLE_ARN="${AWS_ROLE_ARN}"
IMAGE_REPO="nginx"
IMAGE_TAG="v3.0.0"

usage() {
    cat <<EOF
Uso: ./test-operator.sh [COMMAND] [OPTIONS]

Comandos:
  setup        - Instalar Argo Rollouts y desplegar el operador
  create-test  - Crear un Rollout de prueba
  logs         - Ver logs del operador
  describe     - Describir el Rollout y mostrar eventos
  healthy      - Marcar el Rollout como Healthy (simular despliegue exitoso)
  check-tags   - Verificar tags creados en ECR
  clean        - Limpiar namespace de prueba
  full-test    - Ejecutar setup + create-test (flujo completo)

Opciones:
  --account-id ID      - AWS Account ID (default: \$AWS_ACCOUNT_ID)
  --region REGION      - AWS Region (default: us-east-1)
    --role-arn ARN       - Role ARN para IRSA (default: \$AWS_ROLE_ARN)
    --image-repo REPO    - Repositorio ECR sin registry (default: nginx)
  --image-tag TAG      - Tag de imagen (default: v3.0.0)
  --help               - Mostrar esta ayuda

Ejemplos:
  ./test-operator.sh setup --account-id 123456789012 --region us-east-1
  ./test-operator.sh create-test
  ./test-operator.sh logs
  ./test-operator.sh healthy
    ./test-operator.sh check-tags --account-id 123456789012 --image-repo nginx
EOF
}

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

check_requirements() {
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl no encontrado. Instálalo primero."
        exit 1
    fi
    if ! command -v aws &> /dev/null; then
        log_warn "aws CLI no encontrado. No podrás validar tags en ECR."
    fi
}

install_argo() {
    log_info "Instalando Argo Rollouts..."
    kubectl create namespace argo-rollouts --dry-run=client -o yaml | kubectl apply -f -
    kubectl apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/download/v1.8.0/install.yaml
    log_info "Esperando a que Argo Rollouts esté listo..."
    kubectl rollout status deployment/argo-rollouts -n argo-rollouts --timeout=5m
    log_info "✓ Argo Rollouts instalado"
}

deploy_operator() {
    log_info "Desplegando operador ECR Tagger..."
    kubectl apply -f config/operator.yaml

    log_info "Asegurando anotacion IRSA en ServiceAccount..."
    kubectl -n $OPERATOR_NAMESPACE annotate sa rollout-ecr-tagger-controller \
        eks.amazonaws.com/role-arn="$ROLE_ARN" --overwrite

    log_info "Reiniciando deployment para tomar IRSA..."
    kubectl rollout restart deployment/rollout-ecr-tagger-controller -n $OPERATOR_NAMESPACE

    log_info "Esperando a que el operador esté listo..."
    kubectl rollout status deployment/rollout-ecr-tagger-controller \
        -n $OPERATOR_NAMESPACE --timeout=5m

    log_info "Verificando anotacion en ServiceAccount..."
    kubectl -n $OPERATOR_NAMESPACE get sa rollout-ecr-tagger-controller \
        -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}{"\n"}'

    log_info "✓ Operador desplegado"
}

create_test_rollout() {
    if [ -z "$ACCOUNT_ID" ]; then
        log_error "AWS_ACCOUNT_ID no está configurado. Usa: ./test-operator.sh create-test --account-id <ACCOUNT_ID>"
        exit 1
    fi
    
    log_info "Creando namespace de prueba..."
    kubectl create namespace $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -
    
    log_info "Creando Rollout de prueba..."
    kubectl apply -f - <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: test-app
  namespace: $NAMESPACE
  annotations:
    ecr-tagger.io/environment: dev
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test-app
  template:
    metadata:
      labels:
        app: test-app
    spec:
      containers:
      - name: app
        image: $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$IMAGE_REPO:$IMAGE_TAG
        ports:
        - containerPort: 8080
  strategy:
    canary:
      steps:
      - pause: {duration: 1m}
EOF
    
    log_info "✓ Rollout de prueba creado"
    echo ""
    log_info "Rollout details:"
    kubectl get rollout test-app -n $NAMESPACE
}

show_logs() {
    log_info "Mostrando logs del operador (Ctrl+C para salir)..."
    kubectl logs -f deployment/rollout-ecr-tagger-controller -n $OPERATOR_NAMESPACE
}

describe_rollout() {
    log_info "Describiendo Rollout..."
    kubectl describe rollout test-app -n $NAMESPACE || log_warn "Rollout no encontrado"
    echo ""
    log_info "Anotaciones:"
    kubectl get rollout test-app -n $NAMESPACE -o yaml | grep -A 5 "annotations:" || echo "Sin anotaciones"
}

mark_healthy() {
    log_info "Marcando Rollout como Healthy..."
    kubectl patch rollout test-app -n $NAMESPACE \
        -p '{"status":{"phase":"Healthy","observedGeneration":1}}' \
        --type merge
    log_info "✓ Rollout marcado como Healthy"
    echo ""
    log_info "Esperando a que el operador procese..."
    sleep 3
    echo ""
    describe_rollout
}

check_ecr_tags() {
    if [ -z "$ACCOUNT_ID" ]; then
        log_error "AWS_ACCOUNT_ID no está configurado."
        exit 1
    fi
    
    if ! command -v aws &> /dev/null; then
        log_error "aws CLI no encontrado. Instálalo para verificar tags en ECR."
        exit 1
    fi
    
    log_info "Verificando tags en ECR ($REGION / $IMAGE_REPO)..."
    aws ecr describe-images \
        --repository-name $IMAGE_REPO \
        --region $REGION \
        --query 'imageDetails[*].imageTags' \
        --output text 2>/dev/null | tr '\t' '\n' || log_warn "No se pudieron obtener tags"
}

cleanup() {
    log_warn "Eliminando recursos de prueba..."
    kubectl delete namespace $NAMESPACE --ignore-not-found=true
    log_info "✓ Limpieza completada"
}

# Parse arguments
COMMAND="$1"
shift || true

while [[ $# -gt 0 ]]; do
    case $1 in
        --account-id)
            ACCOUNT_ID="$2"
            shift 2
            ;;
        --region)
            REGION="$2"
            shift 2
            ;;
        --role-arn)
            ROLE_ARN="$2"
            shift 2
            ;;
        --image-repo)
            IMAGE_REPO="$2"
            shift 2
            ;;
        --image-tag)
            IMAGE_TAG="$2"
            shift 2
            ;;
        --help)
            usage
            exit 0
            ;;
        *)
            log_error "Opción desconocida: $1"
            usage
            exit 1
            ;;
    esac
done

check_requirements

case "$COMMAND" in
    setup)
        install_argo
        deploy_operator
        ;;
    create-test)
        create_test_rollout
        ;;
    logs)
        show_logs
        ;;
    describe)
        describe_rollout
        ;;
    healthy)
        mark_healthy
        ;;
    check-tags)
        check_ecr_tags
        ;;
    clean)
        cleanup
        ;;
    full-test)
        install_argo
        deploy_operator
        create_test_rollout
        ;;
    *)
        usage
        exit 1
        ;;
esac
