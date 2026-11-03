# Guía de Testing del Operador ECR Tagger

## Prueba local rápida con kind (sin push de imagen)

Requisitos: `docker`, `kind`, `kubectl`, `go` y credenciales AWS locales (perfil o variables de entorno).
La imagen se compila en el host, se carga en kind con `kind load` y el operador toma las credenciales
de un ConfigMap (`rollout-ecr-tagger-aws-local`) vía `envFrom`. **Solo para desarrollo local.**

```bash
export AWS_PROFILE=mi-perfil     # o AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
make kind-test TEST_IMAGE=123456789012.dkr.ecr.us-east-1.amazonaws.com/my-app:v1.0.0
make kind-healthy                # simula el Rollout Healthy y dispara el retag
make kind-logs                   # logs del operador
make kind-tags TEST_IMAGE=...    # tags resultantes en ECR
```

Ciclo de desarrollo: `make kind-redeploy`. Credenciales expiradas (SSO/STS): `make kind-creds`.
Limpieza: `make kind-down`. Todos los targets: `make kind-help`.
Por defecto solo se instala el CRD de Argo Rollouts (sin controlador, sin pods); usa `ARGO_FULL=true` para el controlador completo.

## Requisitos previos

- Cluster de Kubernetes funcionando (`kubectl` configurado y conectado)
- Docker instalado y autenticado en el registro donde publiques (GHCR: `docker login ghcr.io`)
- Credenciales AWS configuradas (IAM user con permisos ECR)
- Una imagen en ECR ya publicada para hacer pruebas

## Paso 1: Instalar Argo Rollouts

```bash
# Crear namespace para Argo Rollouts
kubectl create namespace argo-rollouts

# Instalar Argo Rollouts
kubectl apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/download/v1.8.0/install.yaml

# Verificar que Argo Rollouts está listo
kubectl rollout status deployment/argo-rollouts -n argo-rollouts --timeout=5m
```

## Paso 2: Build y Push de la imagen del operador

```bash
# Desde la raíz del proyecto
docker login ghcr.io  # usuario de GitHub y un token con scope write:packages

# Build de la imagen
make docker-build IMAGE=ghcr.io/henryd24/rollout-ecr-tagger:0.1.0

# Push a GHCR
make docker-push IMAGE=ghcr.io/henryd24/rollout-ecr-tagger:0.1.0

# Opcionalmente, usa 'latest'
make docker-build
make docker-push
```

## Paso 3: Configurar credenciales AWS en el cluster

El operador necesita acceso a ECR. Tienes dos opciones:

### Opción A: IRSA (IAM Roles for Service Accounts) - Recomendado

Si tu cluster está en EKS:

```bash
# Crear una política IAM para ECR
cat > /tmp/ecr-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ecr:BatchGetImage",
        "ecr:PutImage"
      ],
      "Resource": "arn:aws:ecr:*:ACCOUNT_ID:repository/*"
    }
  ]
}
EOF

# Crear rol IAM con la política
aws iam create-role --role-name rollout-ecr-tagger-role \
  --assume-role-policy-document '{...}'

# Asociar rol al ServiceAccount
kubectl annotate serviceaccount rollout-ecr-tagger-controller \
  -n rollout-ecr-tagger-system \
  eks.amazonaws.com/role-arn=arn:aws:iam::ACCOUNT_ID:role/rollout-ecr-tagger-role
```

### Opción B: Usar AWS credentials en Secret

```bash
# Crear un secret con credenciales AWS
kubectl create secret generic aws-credentials \
  -n rollout-ecr-tagger-system \
  --from-literal=AWS_ACCESS_KEY_ID="$(aws configure get aws_access_key_id)" \
  --from-literal=AWS_SECRET_ACCESS_KEY="$(aws configure get aws_secret_access_key)"

# Luego, en el Deployment del operador, agregar:
# env:
# - name: AWS_ACCESS_KEY_ID
#   valueFrom:
#     secretKeyRef:
#       name: aws-credentials
#       key: AWS_ACCESS_KEY_ID
# - name: AWS_SECRET_ACCESS_KEY
#   valueFrom:
#     secretKeyRef:
#       name: aws-credentials
#       key: AWS_SECRET_ACCESS_KEY
```

## Paso 4: Deploy del operador

```bash
# Aplicar el manifiesto del operador
kubectl apply -f config/operator.yaml

# Verificar que el operador está corriendo
kubectl get pods -n rollout-ecr-tagger-system
kubectl logs -f deployment/rollout-ecr-tagger-controller -n rollout-ecr-tagger-system
```

## Paso 5: Crear un Rollout de prueba

```bash
# Crear un namespace para pruebas
kubectl create namespace rollout-test

# Crear el Rollout de prueba
kubectl apply -f - <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: test-app
  namespace: rollout-test
  annotations:
    ecr-tagger.io/environment: staging
spec:
  replicas: 2
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
        image: 123456789012.dkr.ecr.us-east-1.amazonaws.com/my-app:v1.0.0
        ports:
        - containerPort: 8080
  strategy:
    canary:
      steps:
      - setWeight: 50
      - pause: {duration: 5m}
      - setWeight: 100
      - pause: {duration: 5m}
EOF
```

**Nota:** Reemplaza:
- `123456789012` con tu AWS Account ID
- `us-east-1` con tu región
- `my-app` con tu repositorio ECR actual
- `v1.0.0` con un tag existente en tu ECR

## Paso 6: Monitorear y probar

### Verificar que el operador detecta el Rollout

```bash
# Ver anotaciones del Rollout
kubectl get rollout test-app -n rollout-test -o yaml | grep ecr-tagger.io

# Ver logs del operador
kubectl logs -f deployment/rollout-ecr-tagger-controller -n rollout-ecr-tagger-system
```

### Simular un despliegue exitoso

Para que el Rollout pase a estado `Healthy` y el operador retaguee:

```bash
# Opción 1: Esperar a que Argo complete automáticamente (si la app está sana)
# Opción 2: Forzar el estado Healthy con:
kubectl patch rollout test-app -n rollout-test \
  -p '{"status":{"phase":"Healthy"}}' \
  --type merge
```

### Verificar tags en ECR

```bash
# Listar imágenes en el repositorio
aws ecr describe-images \
  --repository-name my-app \
  --region us-east-1 \
  --query 'imageDetails[*].[imageTags,imageSizeInBytes]' \
  --output table

# Deberías ver tags como:
# - staging-v1.0.0
# - active-staging
```

### Ver eventos del operador

```bash
# Eventos del Rollout
kubectl describe rollout test-app -n rollout-test | grep -A 5 "Events:"

# Logs detallados
kubectl logs deployment/rollout-ecr-tagger-controller -n rollout-ecr-tagger-system -f
```

## Paso 7: Limpiar después de probar

```bash
# Eliminar el Rollout de prueba
kubectl delete rollout test-app -n rollout-test

# Eliminar namespace de prueba
kubectl delete namespace rollout-test

# Eliminar el operador (si quieres)
kubectl delete -f config/operator.yaml
```

## Troubleshooting

### El operador no retaguea
- Verifica los logs: `kubectl logs -f deployment/rollout-ecr-tagger-controller -n rollout-ecr-tagger-system`
- Confirma que el Rollout está en estado `Healthy`
- Verifica que las credenciales AWS tienen permisos `ecr:BatchGetImage` y `ecr:PutImage`
- Prueba permisos manualmente: `aws ecr batch-get-image --repository-name my-app --image-ids imageTag=v1.0.0`

### El Rollout no alcanza estado Healthy
- Verifica que los pods están runnning: `kubectl get pods -n rollout-test`
- Ver logs de Argo Rollouts: `kubectl logs -f deployment/argo-rollouts -n argo-rollouts`
- Ajusta o elimina la estrategia canary del Rollout para que sea más simple

### Credenciales AWS no se cargan
- Si usas IRSA, verifica que la anotación está en el ServiceAccount
- Si usas Secret, verifica que el Secret existe: `kubectl get secret aws-credentials -n rollout-ecr-tagger-system`
- Comprueba variables de entorno: `kubectl exec -it pod/... -n rollout-ecr-tagger-system -- env | grep AWS`

## Testing en local contra ECR real

Con kind: ver el README (`make kind-test`). Si no tienes un repositorio ECR de pruebas, [test/terraform/README.md](test/terraform/README.md) crea y destruye uno con Terraform.

## Testing en local (sin cluster)

Para probar el operador sin Kubernetes:

```bash
# Asegúrate que tienes AWS_REGION configurada
export AWS_REGION=us-east-1

# O inicia el operador localmente
go run ./cmd/main.go --leader-elect=false
```

Esto fallará sin un cluster, pero puedes testear la lógica unitaria:

```bash
go test ./... -v
```

## Tests automatizados

```bash
make test               # tests unitarios
make test-integration   # Rollouts, Deployments, workloadRef y --watch-namespaces contra un API server real (envtest)
make helm-lint          # valida el chart
```
