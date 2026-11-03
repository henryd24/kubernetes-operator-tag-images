# Rollout ECR Tagger Operator

Operador de Kubernetes que observa recursos `Rollout` de Argo Rollouts (y opcionalmente `Deployment`). Cuando detecta que un despliegue termino correctamente, retaguea la imagen en ECR con:

- `<ambiente>-<última-parte-del-tag>`: identifica el despliegue con el ambiente y la última parte del tag original (separado por `-`).
- `active-<ambiente>`: puntero mutable a la imagen activa del ambiente.

## Como funciona

1. Observa objetos `argoproj.io/v1alpha1` de tipo `Rollout` y, con `--enable-deployments`, los `Deployment` con el label `ecr-tagger.io/enabled: "true"`.
2. Espera a que el despliegue termine:
   - **Rollout**: `status.phase` es `Healthy`, el `observedGeneration` esta actualizado y no esta pausado.
   - **Deployment**: los mismos criterios que `kubectl rollout status` (todas las replicas actualizadas y disponibles, sin pods viejos, sin `ProgressDeadlineExceeded`). Los Deployments pausados o escalados a 0 se ignoran.
3. Lee la imagen del primer contenedor de la plantilla de pods (o del indicado con `ecr-tagger.io/container`). Para Rollouts con `spec.workloadRef`, la plantilla se lee del Deployment, ReplicaSet o PodTemplate referenciado.
4. Usa `BatchGetImage` y `PutImage` en ECR para crear los tags objetivo.
5. Escribe anotaciones para no retaguear la misma imagen. Solo un cambio de imagen dispara un retag: escalar, reiniciar o cambiar recursos no genera llamadas a ECR.

Si una llamada a AWS falla (permisos, repositorio inexistente, throttling), se reintenta con espera exponencial: desde 5s hasta un maximo de 15 minutos entre intentos.

### Deployments

Desactivado por defecto. Para usarlo:

1. Inicia el operador con `--enable-deployments` (en Helm: `enableDeployments: true`), que ademas agrega los permisos RBAC necesarios.
2. Agrega el label (no anotacion) a cada Deployment que quieras tagear:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  labels:
    ecr-tagger.io/enabled: "true"
  annotations:
    ecr-tagger.io/environment: prod
    ecr-tagger.io/tag-suffix: full
```

Se usa un label porque el API server puede filtrar por labels: el operador solo recibe y guarda en memoria los Deployments marcados, y nunca toca Deployments de sistema (por ejemplo los add-ons de EKS, cuyas imagenes viven en ECR de cuentas de AWS). Las mismas anotaciones de los Rollouts aplican a los Deployments.

No pongas el label en un Deployment referenciado por el `workloadRef` de un Rollout: ese Deployment lo gestiona el Rollout y se tagea a traves de el.

### Repositorios con tags inmutables

Si el repositorio tiene `IMMUTABLE` activo, `active-<ambiente>` no se puede mover una vez creado. El operador lo detecta (`ImageTagAlreadyExistsException`), aplica el resto de tags (el de despliegue si es nuevo), emite un evento `ECRTagImmutable` y marca la imagen como procesada: no reintenta, porque nunca podria tener exito. No requiere permisos IAM adicionales.

## Anotaciones soportadas (Rollout y Deployment)

- `ecr-tagger.io/environment` (opcional): nombre del ambiente. Si no existe, usa el namespace.
- `ecr-tagger.io/repository` (opcional): repositorio ECR destino para el retag.
- `ecr-tagger.io/account-id` (opcional): cuenta AWS del registry destino (por defecto, la de la imagen).
- `ecr-tagger.io/container` (opcional): nombre del contenedor cuya imagen se tagea. Por defecto, el primero.
- `ecr-tagger.io/skip` (opcional): `"true"` desactiva el tagging para ese recurso.
- `ecr-tagger.io/tag-suffix` (opcional): `last-segment` (por defecto) usa la ultima parte del tag tras `-`; `full` usa el tag completo (`prod-v1.8.4-alpha`), evitando colisiones.

Anotaciones internas usadas por el operador:

- `ecr-tagger.io/last-tagged-image`
- `ecr-tagger.io/last-tagged-generation`

Label:

- `ecr-tagger.io/enabled: "true"`: necesario en los Deployments (no aplica a Rollouts).

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
| `--max-concurrent-reconciles` | `1` | Recursos procesados en paralelo (por tipo). |
| `--enable-deployments` | `false` | Tambien tagea Deployments con el label `ecr-tagger.io/enabled=true`. |
| `--watch-namespaces` | (todos) | Lista separada por comas de namespaces a observar. |
| `--zap-log-level` | `info` | Nivel de log (`debug`, `info`, `error`). |
| `--version` | | Imprime la version y termina. |

## Metricas

Ademas de las metricas estandar de controller-runtime, se expone:

- `ecr_tagger_tag_operations_total{kind,namespace,result}`: intentos de tagging (`kind` = `rollout` o `deployment`; `result` = `success`, `failure` o `immutable`).

## Limpieza del repositorio

El tag `active-<ambiente>` permite usar lifecycle policies de ECR que borran imagenes viejas sin tocar las que estan corriendo. Ver [docs/ecr-lifecycle-policy.md](docs/ecr-lifecycle-policy.md).

## Desarrollo local

```bash
go mod tidy
make test               # tests unitarios
make test-integration   # tests contra un API server real (envtest; descarga los binarios)
make helm-lint          # requiere helm
go run ./cmd/main.go --leader-elect=false
```

### Probar en un cluster local con kind

Flujo rapido para probar el operador contra ECR real sin publicar ninguna imagen: el binario se compila en tu maquina, se carga en kind con `kind load` y las credenciales AWS locales se guardan en un ConfigMap (`rollout-ecr-tagger-aws-local`) que el operador lee via `envFrom`.

Requisitos: `docker`, `kind`, `kubectl`, `go` y credenciales AWS (perfil o variables de entorno). La imagen indicada en `TEST_IMAGE` debe existir ya en ECR.

```bash
export AWS_PROFILE=mi-perfil   # o AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
make kind-test TEST_IMAGE=123456789012.dkr.ecr.us-east-1.amazonaws.com/my-app:v1.0.0
make kind-healthy              # simula el Rollout Healthy y dispara el retag
make kind-logs                 # logs del operador
make kind-tags TEST_IMAGE=123456789012.dkr.ecr.us-east-1.amazonaws.com/my-app:v1.0.0
```

| Target | Que hace |
|---|---|
| `kind-test` | Flujo completo: cluster, CRD de Rollouts, build, credenciales, deploy y Rollout de prueba |
| `kind-redeploy` | Recompila y redespliega el operador tras cambiar codigo |
| `kind-creds` | Refresca el ConfigMap de credenciales y reinicia el operador (util con SSO/STS expiradas) |
| `kind-events` | Muestra el Rollout de prueba con sus anotaciones y eventos |
| `kind-down` | Elimina el cluster kind |
| `kind-help` | Lista todos los targets y variables |

Por defecto solo se instala el CRD de Argo Rollouts (sin controlador ni pods); usa `ARGO_FULL=true` para instalar el controlador completo. Variables utiles: `KIND_CLUSTER`, `KIND_IMAGE`, `AWS_REGION`, `TARGET_ROLE_ARN`, `TEST_NAMESPACE`, `TEST_ROLLOUT`.

> Un ConfigMap guarda las credenciales en texto plano: usalo solo en clusters locales de desarrollo. Mas detalle en [TESTING.md](TESTING.md).

## Build

```bash
make build
make docker-build IMAGE=ghcr.io/henryd24/rollout-ecr-tagger:v0.5.0 VERSION=v0.5.0
```

## Release

El release tiene dos pasos y la imagen se publica en GitHub Container Registry (`ghcr.io/henryd24/rollout-ecr-tagger`):

1. **Preparar (manual).** En Actions, ejecuta el workflow `Release` sobre `trunk` y elige `patch`, `minor` o `major`; la versión se calcula a partir de la actual del chart (p. ej. `0.5.0` + `minor` = `0.6.0`). Abre el PR `release/v0.6.0` que sube `version` y `appVersion` del chart, `image.repository` y la imagen de `config/operator.yaml`.
2. **Publicar (automático).** Al mergear ese PR a `trunk`, el workflow `Publish` construye y sube la imagen multi-arquitectura (`linux/amd64`, `linux/arm64`) a GHCR y crea el GitHub Release `v0.6.0` con el chart empaquetado.

Requisitos: en Settings > Actions > General, activar «Allow GitHub Actions to create and approve pull requests». El PR lo abre `GITHUB_TOKEN`, así que no dispara el CI por sí solo; ciérralo y reábrelo para que corra. La primera vez, hay que hacer público el paquete en GHCR (Packages > Package settings) para que los clusters puedan descargar la imagen sin credenciales.


## Deploy con Helm (recomendado)

```bash
helm install rollout-ecr-tagger charts/rollout-ecr-tagger \
  --namespace rollout-ecr-tagger-system --create-namespace \
  --set aws.region=us-east-1 \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::<ACCOUNT_ID>:role/<ROLE_NAME>
```

Valores principales (ver [values.yaml](charts/rollout-ecr-tagger/values.yaml)):

| Valor | Default | Descripcion |
|-------|---------|-------------|
| `aws.region` | `us-east-1` | Region por defecto. |
| `aws.targetRoleArn` | `""` | Rol a asumir para tagear en otra cuenta. |
| `serviceAccount.annotations` | `{}` | Anotacion de IRSA. |
| `enableDeployments` | `false` | Habilita Deployments y sus permisos RBAC. |
| `watchNamespaces` | `[]` | Si se define, usa `Role` por namespace en lugar de `ClusterRole`. |
| `metrics.serviceMonitor.enabled` | `false` | Crea un `ServiceMonitor` para Prometheus Operator. |

## Deploy con manifiesto

1. Actualiza la imagen en `config/operator.yaml`.
2. Configura las credenciales de AWS: la anotacion de IRSA en el `ServiceAccount` y, solo si tageas en otra cuenta, `TARGET_ROLE_ARN` en el `ConfigMap` (vacio por defecto).
3. Para Deployments, descomenta las reglas RBAC y el flag `--enable-deployments`.
4. Aplica el manifiesto:

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
