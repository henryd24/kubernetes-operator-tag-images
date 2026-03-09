# Rollout ECR Tagger Operator

Operador de Kubernetes que observa recursos `Rollout` de Argo Rollouts. Cuando detecta que un rollout paso a estado `Healthy`, retaguea la imagen en ECR con:

- `<ambiente>-<última-parte-del-tag>`: identifica el despliegue con el ambiente y la última parte del tag original (separado por `-`).
- `active-<ambiente>`: puntero mutable a la imagen activa del ambiente.

## Como funciona

1. Observa objetos `argoproj.io/v1alpha1` de tipo `Rollout`.
2. Verifica que `status.phase` sea `Healthy` y que el `observedGeneration` este actualizado.
3. Lee la imagen del primer contenedor en `spec.template.spec.containers[0].image`.
4. Usa `BatchGetImage` y `PutImage` en ECR para crear los tags objetivo.
5. Escribe anotaciones para no retaguear la misma imagen/generacion repetidamente.

## Anotaciones soportadas en Rollout

- `ecr-tagger.io/environment` (opcional): nombre del ambiente. Si no existe, usa el namespace.
- `ecr-tagger.io/repository` (opcional): repositorio ECR destino para el retag.

Anotaciones internas usadas por el operador:

- `ecr-tagger.io/last-tagged-image`
- `ecr-tagger.io/last-tagged-generation`

## Requisitos

- CRD de Argo Rollouts instalado en el cluster.
- Permisos AWS para `ecr:BatchGetImage` y `ecr:PutImage`.
- Las imagenes de Rollout deben apuntar a ECR (`*.dkr.ecr.<region>.amazonaws.com/repo:tag`).

## Desarrollo local

```bash
go mod tidy
go test ./...
go run ./cmd/main.go --leader-elect=false
```

## Build

```bash
make build
make docker-build IMAGE=henryd24/rollout-ecr-tagger:0.1.0
make docker-push IMAGE=henryd24/rollout-ecr-tagger:0.1.0
```

## Deploy

1. Actualiza la imagen en `config/operator.yaml` (o deja `henryd24/rollout-ecr-tagger:latest`).
2. Aplica el manifiesto:

```bash
kubectl apply -f config/operator.yaml
```

## Ejemplo de Rollout

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: payment-api
  namespace: prod
  annotations:
    ecr-tagger.io/environment: prod
spec:
  replicas: 3
  selector:
    matchLabels:
      app: payment-api
  template:
    metadata:
      labels:
        app: payment-api
    spec:
      containers:
      - name: payment-api
        image: 123456789012.dkr.ecr.us-east-1.amazonaws.com/payment-api:v1.8.4
```

## Ejemplo de tags generados

Con el Rollout anterior, si la imagen es `payment-api:v1.8.4`:
- Tag generado: `prod-v1.8.4` (ambiente `prod` + última parte del tag `v1.8.4`)
- Tag activo: `active-prod` (siempre apunta a la versión activa en prod)

Si el tag fuera `v1.8.4-alpha`:
- Tag generado: `prod-alpha` (se toma `alpha` que es la última parte después del split por `-`)
