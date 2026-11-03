#!/usr/bin/env bash
# Imprime (KEY=VALUE) las credenciales AWS locales para guardarlas en un ConfigMap de kind.
# Solo escribe a stdout; no deja nada en disco.
#
# Orden de resolución:
#   1. Variables de entorno AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (/ AWS_SESSION_TOKEN)
#   2. `aws configure export-credentials` con AWS_PROFILE (soporta SSO, assume-role, etc.)
set -euo pipefail

profile_args=()
[ -z "${AWS_PROFILE:-}" ] || profile_args=(--profile "$AWS_PROFILE")

region="${AWS_REGION:-${AWS_DEFAULT_REGION:-}}"
if [ -z "$region" ] && command -v aws >/dev/null 2>&1; then
  region="$(aws configure get region "${profile_args[@]}" 2>/dev/null || true)"
fi
region="${region:-us-east-1}"

if [ -z "${AWS_ACCESS_KEY_ID:-}" ]; then
  if ! command -v aws >/dev/null 2>&1; then
    echo "No hay AWS_ACCESS_KEY_ID exportada y 'aws' CLI no está instalado." >&2
    exit 1
  fi
  if ! creds="$(aws configure export-credentials "${profile_args[@]}" --format env-no-export 2>/dev/null)"; then
    echo "No se pudieron obtener credenciales (perfil: ${AWS_PROFILE:-default})." >&2
    echo "Haz 'aws sso login --profile <perfil>' o exporta AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY." >&2
    exit 1
  fi
  printf '%s\n' "$creds"
else
  : "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY no está definida}"
  echo "AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID}"
  echo "AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY}"
  [ -z "${AWS_SESSION_TOKEN:-}" ] || echo "AWS_SESSION_TOKEN=${AWS_SESSION_TOKEN}"
fi
echo "AWS_REGION=${region}"
echo "TARGET_ROLE_ARN=${TARGET_ROLE_ARN:-}"
