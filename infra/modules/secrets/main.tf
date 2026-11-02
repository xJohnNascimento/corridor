terraform {
  required_version = ">= 1.11"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 6.0" }
    random = { source = "hashicorp/random", version = "~> 3.7" }
  }
}

variable "prefix" {
  type = string
}

variable "generated" {
  type        = map(string)
  default     = {}
  description = "Secret name to a value prefix; the value is the prefix and 48 random alphanumerics, which is also valid base64"
}

variable "external" {
  type        = set(string)
  default     = []
  description = "Secrets an operator fills after apply, such as keys minted by another system"
}

variable "rotation" {
  type        = number
  default     = 1
  description = "Raising it rewrites every generated secret on the next apply"
}

resource "aws_secretsmanager_secret" "this" {
  for_each = setunion(keys(var.generated), var.external)
  name     = "${var.prefix}/${each.key}"
}

# Ephemeral values and write-only arguments keep the generated secrets out of plans and state.
ephemeral "random_password" "this" {
  for_each = var.generated
  length   = 48
  special  = false
}

resource "aws_secretsmanager_secret_version" "this" {
  for_each                 = var.generated
  secret_id                = aws_secretsmanager_secret.this[each.key].id
  secret_string_wo         = "${each.value}${ephemeral.random_password.this[each.key].result}"
  secret_string_wo_version = var.rotation
}

output "arns" {
  value = { for name, secret in aws_secretsmanager_secret.this : name => secret.arn }
}
