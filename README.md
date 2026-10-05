# Rollout ECR Tagger Operator

Operador de Kubernetes que observa recursos `Rollout` de Argo Rollouts. Cuando detecta que un rollout paso a estado `Healthy`, retaguea la imagen en ECR con:

- `<ambiente>-<última-parte-del-tag>`: identifica el despliegue con el ambiente y la última parte del tag original (separado por `-`).
- `active-<ambiente>`: puntero mutable a la imagen activa del ambiente.

## Como funciona

1. Observa objetos `argoproj.io/v1alpha1` de tipo `Rollout`.
2. Verifica que `status.phase` sea `Healthy` y que el `observedGeneration` este actualizado.
3. Lee la imagen del primer contenedor en `spec.template.spec.containers[0].image` (o del indicado con `ecr-tagger.io/container`).
4. Usa `BatchGetImage` y `PutImage` en ECR para crear los tags objetivo.
5. Escribe anotaciones para no retaguear la misma imagen. Solo un cambio de imagen dispara un retag: escalar, reiniciar o cambiar recursos del Rollout no genera llamadas a ECR.

### Repositorios con tags inmutables

Si el repositorio tiene `IMMUTABLE` activo, `active-<ambiente>` no se puede mover una vez creado. El operador lo detecta (`ImageTagAlreadyExistsException`), aplica el resto de tags (el de despliegue si es nuevo), emite un evento `ECRTagImmutable` y marca la imagen como procesada: no reintenta, porque nunca podria tener exito. No requiere permisos IAM adicionales.

## Anotaciones soportadas en Rollout

- `ecr-tagger.io/environment` (opcional): nombre del ambiente. Si no existe, usa el namespace.
- `ecr-tagger.io/repository` (opcional): repositorio ECR destino para el retag.
- `ecr-tagger.io/account-id` (opcional): cuenta AWS del registry destino (por defecto, la de la imagen).
- `ecr-tagger.io/container` (opcional): nombre del contenedor cuya imagen se tagea. Por defecto, el primero.
- `ecr-tagger.io/skip` (opcional): `"true"` desactiva el tagging para ese Rollout.
- `ecr-tagger.io/tag-suffix` (opcional): `last-segment` (por defecto) usa la ultima parte del tag tras `-`; `full` usa el tag completo (`prod-v1.8.4-alpha`), evitando colisiones.

Anotaciones internas usadas por el operador:

- `ecr-tagger.io/last-tagged-image`
- `ecr-tagger.io/last-tagged-generation`

## Requisitos

- CRD de Argo Rollouts instalado en el cluster.
- Permisos AWS para `ecr:BatchGetImage` y `ecr:PutImage`.
- Las imagenes de Rollout deben apuntar a ECR (`*.dkr.ecr.<region>.amazonaws.com/repo:tag`). Tambien se soportan endpoints FIPS, China y dual-stack. Las imagenes que no son de ECR se ignoran.
- Se soportan imagenes multi-arquitectura (manifest list / OCI index).

## Flags

| Flag | Default | Descripcion |
|------|---------|-------------|
| `--leader-elect` | `true` | Habilita leader election. |
| `--metrics-bind-address` | `:8080` | Direccion del endpoint de metricas. |
| `--health-probe-bind-address` | `:8081` | Direccion de los probes. |
| `--max-concurrent-reconciles` | `1` | Rollouts procesados en paralelo. |
| `--version` | | Imprime la version y termina. |

## Metricas

Ademas de las metricas estandar de controller-runtime, se expone:

- `ecr_tagger_tag_operations_total{namespace,result}`: intentos de tagging (`result` = `success`, `failure` o `immutable`).

## Desarrollo local

```bash
go mod tidy
go test ./...
go run ./cmd/main.go --leader-elect=false
```

## Build

```bash
make build
make docker-build IMAGE=henda24/rollout-ecr-tagger:v0.5.0 VERSION=v0.5.0
make docker-push IMAGE=henda24/rollout-ecr-tagger:v0.5.0
```

## Deploy

1. Actualiza la imagen en `config/operator.yaml` (o deja `henda24/rollout-ecr-tagger:latest`).
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
    ecr-tagger.io/tag-suffix: full   # recomendado: evita colisiones del tag de despliegue
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
- Con `ecr-tagger.io/tag-suffix: full`: `prod-v1.8.4-alpha`
- Sin la anotación (modo por defecto): `prod-alpha` (se toma `alpha` que es la última parte después del split por `-`)

Ojo: con el modo por defecto, `v1.8.4-alpha` y `v1.9.0-alpha` generan ambos `prod-alpha`. Como el tag de despliegue nunca se sobrescribe, el segundo despliegue no recibe tag propio y `prod-alpha` sigue apuntando a `v1.8.4-alpha`. Cuando esto ocurre el operador emite un evento `DeploymentTagCollision`. Si tus tags no terminan en un identificador unico (p. ej. el SHA del commit), usa `ecr-tagger.io/tag-suffix: full`.
