# ECR temporal para `make kind-test`

El flujo `make kind-*` (ver [README](../../README.md#probar-en-un-cluster-local-con-kind))
necesita una imagen que ya exista en ECR. Este Terraform crea una de forma
desechable y fácil de borrar:

- un repositorio ECR `rollout-ecr-tagger-test/test-app` (con `force_delete`);
- un usuario IAM de mínimo privilegio (solo `ecr:BatchGetImage` y `ecr:PutImage`
  sobre ese repo) y su access key. Así el ConfigMap de kind guarda esta clave
  limitada y no tus credenciales de administración.

## Requisitos

`terraform`, `aws`, `docker`, `kind`, `kubectl`, `go` y
[`crane`](https://github.com/google/go-containerregistry)
(`go install github.com/google/go-containerregistry/cmd/crane@latest`) para subir
la imagen de prueba sin necesitar el daemon de Docker.

## 1. Crear el ECR y subir una imagen de prueba

Con credenciales que puedan crear ECR e IAM (p. ej. exportadas desde un `.env`
fuera del repo):

```bash
cd test/terraform
terraform init && terraform apply

R=$(terraform output -raw repository_url)
export DOCKER_CONFIG=$(mktemp -d)    # no toca tu ~/.docker/config.json
aws ecr get-login-password --region us-east-1 \
  | crane auth login "${R%%/*}" -u AWS --password-stdin
crane copy public.ecr.aws/docker/library/busybox:1.36 "$R:v1.0.0"
```

## 2. Probar con kind

Exporta la clave del usuario IAM mínimo (no la de administración) y lanza el flujo:

```bash
export AWS_ACCESS_KEY_ID=$(terraform output -raw operator_access_key_id)
export AWS_SECRET_ACCESS_KEY=$(terraform output -raw operator_secret_access_key)
export AWS_REGION=us-east-1
cd ../..
make kind-test TEST_IMAGE="$R:v1.0.0"
make kind-healthy
make kind-tags TEST_IMAGE="$R:v1.0.0"
```

Tras cambiar código: `make kind-redeploy`.

## 3. Limpiar

```bash
make kind-down
cd test/terraform && terraform destroy   # con las credenciales de administración
```
