# Lifecycle policies de ECR junto con el operador

Las lifecycle policies de ECR borran imagenes viejas automaticamente. Combinadas con
este operador permiten limpiar el repositorio **sin borrar nunca la imagen que esta
corriendo** en un ambiente, porque esa imagen siempre tiene el tag `active-<ambiente>`.

## Politica de ejemplo

[`examples/ecr-lifecycle-policy.json`](../examples/ecr-lifecycle-policy.json):

| Prioridad | Regla | Efecto |
|-----------|-------|--------|
| 1 | `tagPatternList: ["active-*"]`, mas de 9999 imagenes | Selecciona las imagenes activas sin expirarlas nunca (no habra 9999). |
| 2 | Sin tag y con mas de 14 dias | Limpia imagenes huerfanas. |
| 3 | Cualquier imagen, conservar las 50 mas recientes | Limita el tamaño del repositorio. |

### Por que funciona la regla 1

Segun las [reglas de evaluacion de ECR](https://docs.aws.amazon.com/AmazonECR/latest/userguide/LifecyclePolicies.html#lp_evaluation_rules):

- *"An image that matches the tagging requirements of a rule cannot be expired or archived
  by a rule with a lower priority."* La regla 1 selecciona las imagenes `active-*` y nunca
  las expira, asi que las reglas 2 y 3 ya no pueden tocarlas.
- *"If an image is referenced by a manifest list, it cannot be expired or archived without
  the manifest list being deleted or archived first."* Las imagenes por arquitectura de una
  imagen multi-arch (que no tienen tag) estan a salvo mientras el indice tenga `active-*`.

Ajusta el `countNumber` de la regla 3: debe ser mayor que la cantidad de versiones a las
que quieras poder hacer rollback rapido.

## Aplicarla

Siempre revisa el resultado con el preview antes de aplicar:

```bash
REPO=my-app
aws ecr start-lifecycle-policy-preview --repository-name "$REPO" \
  --lifecycle-policy-text file://examples/ecr-lifecycle-policy.json
aws ecr get-lifecycle-policy-preview --repository-name "$REPO"   # revisar que expira

aws ecr put-lifecycle-policy --repository-name "$REPO" \
  --lifecycle-policy-text file://examples/ecr-lifecycle-policy.json
```

## Repositorios inmutables

En un repositorio con `IMMUTABLE`, `active-<ambiente>` no se puede mover despues de crearlo
(el operador emite el evento `ECRTagImmutable`), por lo que no sirve para proteger la imagen
actual. Si usas inmutabilidad, considera `IMMUTABLE_WITH_EXCLUSION` excluyendo el patron
`active-*`, o protege por el prefijo de los tags de despliegue (`prod-*`, `staging-*`).
