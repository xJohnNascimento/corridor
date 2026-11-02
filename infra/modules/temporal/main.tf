terraform {
  required_version = ">= 1.11"
  required_providers {
    temporalcloud = { source = "temporalio/temporalcloud", version = "~> 1.0" }
  }
}

variable "name" {
  type = string
}

variable "region" {
  type    = string
  default = "aws-us-east-1"
}

# API-key auth keeps certificates out of the stack; an operator mints the key for a service
# account and stores it in Secrets Manager, so the key never enters Terraform state.
resource "temporalcloud_namespace" "this" {
  name           = var.name
  regions        = [var.region]
  api_key_auth   = true
  retention_days = 7
}

resource "temporalcloud_namespace_search_attribute" "this" {
  for_each     = { PaymentStatus = "keyword", CustomerId = "keyword", InReview = "bool", ReviewSince = "datetime" }
  namespace_id = temporalcloud_namespace.this.id
  name         = each.key
  type         = each.value
}

output "namespace" {
  value = temporalcloud_namespace.this.id
}

output "endpoint" {
  value = temporalcloud_namespace.this.endpoints.grpc_address
}
