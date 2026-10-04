# Changelog

## v0.5.0 (sin publicar)

Todos los cambios son retrocompatibles: los Rollouts existentes siguen generando
exactamente los mismos tags, y los permisos IAM requeridos no cambian
(`ecr:BatchGetImage`, `ecr:PutImage`).

### Correcciones

- **Imágenes multi-arquitectura**: ahora se pueden retaguear imágenes cuyo manifiesto es un
  manifest list de Docker o un OCI image index. Antes ECR respondía sin imagen y el operador
  reintentaba cada 30s indefinidamente.
- **Imágenes que no son de ECR** (Docker Hub, GHCR, etc.): se ignoran en vez de entrar en un
  ciclo infinito de reintentos con llamadas fallidas a AWS y eventos `ECRTagFailed` cada 30s.
- **Referencias `repo:tag@sha256:...`**: se parsean correctamente (antes el repositorio quedaba
  como `repo:tag` y el retag fallaba). Se usa el digest como fuente y el tag para el sufijo.
- **Caracteres inválidos en tags** (p. ej. `+` de semver `1.0.0+build.5`): se reemplazan por `-`
  en vez de fallar en `PutImage`. Los tags que antes eran válidos no cambian.
- **Repositorios con tags inmutables**: el error `ImageTagAlreadyExistsException` ya no provoca
  reintentos infinitos cada 30s. Se aplican los tags posibles, se emite un evento `ECRTagImmutable`,
  se cuenta en la métrica con `result="immutable"` y la imagen se marca como procesada.
- **Sin llamadas a ECR al escalar**: el retag solo se dispara cuando cambia la imagen. Antes,
  cualquier cambio de `generation` (escalado por HPA, `kubectl argo rollouts restart`, cambios de
  recursos) volvía a llamar a ECR. La anotación `last-tagged-generation` se sigue escribiendo
  como dato informativo.
- **Colisiones del tag de despliegue**: si el tag de despliegue ya existe apuntando a otra imagen
  (p. ej. `v1.8.4-alpha` y `v1.9.0-alpha` → `prod-alpha`) se emite un evento
  `DeploymentTagCollision` en vez de omitirlo en silencio.
- Los errores de `BatchGetImage` ahora incluyen el código y motivo de ECR
  (`ImageNotFound`, `RepositoryNotFound`, ...).

### Nuevas funcionalidades (opt-in)

- Anotación `ecr-tagger.io/skip: "true"`: desactiva el tagging para un Rollout.
- Anotación `ecr-tagger.io/tag-suffix: full`: usa el tag completo como sufijo del tag de
  despliegue (`prod-v1.9.0-alpha`). Por defecto se mantiene el comportamiento anterior.
- Anotación `ecr-tagger.io/container: <nombre>`: elige qué contenedor tagear (por defecto,
  el primero, igual que antes).
- Soporte para registries ECR FIPS (`dkr.ecr-fips`), China (`amazonaws.com.cn`) y dual-stack
  (`dkr-ecr.<region>.on.aws`).
- Métrica Prometheus `ecr_tagger_tag_operations_total{namespace,result}` (`success`, `failure`, `immutable`).
- Flag `--max-concurrent-reconciles` (por defecto `1`, igual que antes).
- Flag `--version` y versión en el log de arranque (inyectada con `-ldflags`).

### Mantenimiento

- Liberación del lease de leader election al apagar (failover más rápido en rolling updates).
- Build reproducible y binario más pequeño (`-trimpath -ldflags "-s -w"`).
- Workflow de CI en GitHub Actions (gofmt, vet, tests con `-race`, build).
- Nuevos targets `make vet` y `make fmt-check`.
