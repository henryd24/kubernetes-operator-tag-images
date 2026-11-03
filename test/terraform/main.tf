# Infraestructura TEMPORAL para probar el operador contra ECR real.
# Crear:   terraform apply
# Borrar:  terraform destroy   (force_delete elimina el repo aunque tenga imágenes)

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "repository_name" {
  type    = string
  default = "rollout-ecr-tagger-test/test-app"
}

provider "aws" {
  region = var.region
}

data "aws_caller_identity" "current" {}

resource "aws_ecr_repository" "test" {
  name                 = var.repository_name
  image_tag_mutability = "MUTABLE"
  force_delete         = true
}

# Usuario IAM de mínimo privilegio solo para el operador (así la clave de
# administración del .env no se copia al clúster).
resource "aws_iam_user" "operator" {
  name          = "rollout-ecr-tagger-test"
  force_destroy = true
}

resource "aws_iam_user_policy" "operator" {
  name = "ecr-tagging"
  user = aws_iam_user.operator.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ecr:BatchGetImage", "ecr:PutImage"]
      Resource = aws_ecr_repository.test.arn
    }]
  })
}

resource "aws_iam_access_key" "operator" {
  user = aws_iam_user.operator.name
}

output "account_id" {
  value = data.aws_caller_identity.current.account_id
}

output "repository_url" {
  value = aws_ecr_repository.test.repository_url
}

output "operator_access_key_id" {
  value = aws_iam_access_key.operator.id
}

output "operator_secret_access_key" {
  value     = aws_iam_access_key.operator.secret
  sensitive = true
}
