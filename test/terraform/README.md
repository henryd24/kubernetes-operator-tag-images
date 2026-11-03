# Pruebas locales contra ECR real

Infraestructura **temporal** en AWS para probar el operador sin construir ni
publicar una imagen: el operador corre en tu máquina (`go run` / binario) y habla
con un clúster y con ECR reales. Así puedes cambiar código y reiniciar en segundos.

Crea con Terraform:

- un repositorio ECR (`rollout-ecr-tagger-test/test-app`, con `force_delete`);
- un usuario IAM de mínimo privilegio (solo `ecr:BatchGetImage` y `ecr:PutImage`
  sobre ese repo) y su access key, para no usar tus credenciales de administración.

## Requisitos

`terraform`, `aws`, `kubectl`, `go` y [`crane`](https://github.com/google/go-containerregistry)
(`go install github.com/google/go-containerregistry/cmd/crane@latest`). No hace
falta Docker: `crane` sube la imagen de prueba sin daemon.

Un clúster accesible con `kubectl`, y credenciales AWS con permisos para crear
ECR e IAM, por ejemplo en un `.env` (nunca lo subas al repo):

```bash
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
```

## 1. Crear el ECR y subir una imagen de prueba

```bash
set -a; source .env; set +a          # credenciales de administración
cd test/terraform
terraform init && terraform apply

R=$(terraform output -raw repository_url)
export DOCKER_CONFIG=$(mktemp -d)    # evita tocar tu ~/.docker/config.json
aws ecr get-login-password --region us-east-1 \
  | crane auth login "${R%%/*}" -u AWS --password-stdin
crane copy public.ecr.aws/docker/library/busybox:1.36 "$R:v1.0.0"
```

## 2. Preparar el clúster

```bash
kubectl create namespace rollout-test
# Los nodos necesitan credenciales para bajar la imagen (token válido 12 h):
kubectl -n rollout-test create secret docker-registry ecr-pull \
  --docker-server="${R%%/*}" --docker-username=AWS \
  --docker-password="$(aws ecr get-login-password --region us-east-1)"
```

Solo para probar **Rollouts**, instala Argo Rollouts (el operador siempre
registra su controlador y, sin el CRD, llena el log de `no matches for kind "Rollout"`):

```bash
kubectl create namespace argo-rollouts
kubectl apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/download/v1.8.0/install.yaml
```

## 3. Ejecutar el operador en local

Usa la clave del usuario IAM mínimo, no la de administración:

```bash
export AWS_ACCESS_KEY_ID=$(terraform output -raw operator_access_key_id)
export AWS_SECRET_ACCESS_KEY=$(terraform output -raw operator_secret_access_key)
export AWS_REGION=us-east-1
cd ../..
go run ./cmd/main.go --leader-elect=false --enable-deployments \
  --watch-namespaces=rollout-test \
  --metrics-bind-address=:18080 --health-probe-bind-address=:18081
```

Tras cambiar código, detén (`Ctrl+C`) y vuelve a lanzar el mismo comando.

## 4. Workloads de prueba

Sustituye `$R` por la URL del repositorio. `command: sleep` evita que busybox
termine y el workload nunca quede sano.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: rollout-test
  labels:
    ecr-tagger.io/enabled: "true"      # obligatorio para Deployments
  annotations:
    ecr-tagger.io/environment: staging
spec:
  replicas: 1
  selector: {matchLabels: {app: test-app}}
  template:
    metadata: {labels: {app: test-app}}
    spec:
      imagePullSecrets: [{name: ecr-pull}]
      containers:
      - name: app
        image: $R:v1.0.0
        command: ["sleep", "3600"]
---
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: test-rollout
  namespace: rollout-test
  annotations:
    ecr-tagger.io/environment: production
spec:
  replicas: 1
  selector: {matchLabels: {app: test-rollout}}
  strategy:
    canary:
      steps: [{setWeight: 100}]
  template:
    metadata: {labels: {app: test-rollout}}
    spec:
      imagePullSecrets: [{name: ecr-pull}]
      containers:
      - name: app
        image: $R:v1.0.0
        command: ["sleep", "3600"]
```

Cuando el workload queda sano, el log del operador muestra
`successfully tagged image in ECR` y ECR contiene los tags:

```bash
aws ecr describe-images --repository-name rollout-ecr-tagger-test/test-app \
  --region us-east-1 --query 'imageDetails[].imageTags' --output json
# staging-v1.0.0, active-staging, production-v1.0.0, active-production
```

Para probar una nueva versión, etiqueta otra imagen y actualiza el workload:

```bash
crane tag "$R:v1.0.0" v1.1.0
kubectl -n rollout-test set image deploy/test-app app="$R:v1.1.0"
```

## Problemas frecuentes

- **Pod en `Pending` con `untolerated taint`**: tu clúster tiene taints en los
  nodos. Añade `tolerations` a la plantilla del pod.
- **`ImagePullBackOff`**: el token de `ecr-pull` expiró (12 h); vuelve a crear el secret.
- **`AccessDenied` en el operador**: estás usando una clave sin permisos sobre el repo.
- **El Deployment no se etiqueta**: falta la etiqueta `ecr-tagger.io/enabled=true`
  o no está completamente desplegado (el operador espera a que todas las réplicas estén disponibles).

## 5. Limpiar

```bash
kubectl delete namespace rollout-test
kubectl delete namespace argo-rollouts   # si lo instalaste
cd test/terraform && terraform destroy   # con las credenciales de administración
```
