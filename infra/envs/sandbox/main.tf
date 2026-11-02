terraform {
  required_version = ">= 1.11"
  required_providers {
    aws           = { source = "hashicorp/aws", version = "~> 6.0" }
    temporalcloud = { source = "temporalio/temporalcloud", version = "~> 1.0" }
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "corridor", Environment = "sandbox", ManagedBy = "terraform" }
  }
}

provider "temporalcloud" {}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "image" {
  type        = string
  description = "The fx image, pinned by digest"
}

variable "certificate_arn" {
  type = string
}

data "aws_ssm_parameter" "foundation" {
  for_each = toset(["vpc-id", "vpc-cidr", "private-subnet-ids", "public-subnet-ids", "cluster-arn", "namespace-id", "oidc-provider-arn"])
  name     = "/sandbox/foundation/${each.key}"
}

# The ledger and Ditto own the secrets they verify and publish where to find them.
data "aws_ssm_parameter" "dependency" {
  for_each = toset(["ledger/endpoint", "ledger/clients/corridor-worker/secret-arn", "ditto/endpoint", "ditto/clients/corridor/key-arn", "ditto/clients/corridor/webhook-secret-arn"])
  name     = "/sandbox/${each.key}"
}

locals {
  foundation      = { for key, parameter in data.aws_ssm_parameter.foundation : key => nonsensitive(parameter.value) }
  dependency      = { for key, parameter in data.aws_ssm_parameter.dependency : key => nonsensitive(parameter.value) }
  private_subnets = split(",", local.foundation["private-subnet-ids"])
  database        = "postgres://%s@${module.database.endpoint}:5432/${module.database.database}?sslmode=require"
  passwords = {
    FX_APP_PASSWORD   = module.secrets.arns["app-password"]
    FX_AUDIT_PASSWORD = module.secrets.arns["audit-password"]
  }
  environment = {
    FX_PROFILE            = "production"
    FX_ENVIRONMENT        = "sandbox"
    FX_OPERATORS          = "alice,bob"
    FX_DATASET_ID         = "sandbox"
    FX_DATABASE_URL       = format(local.database, "fx_app")
    FX_AUDIT_DATABASE_URL = format(local.database, "fx_audit")
    FX_LEDGER_URL         = local.dependency["ledger/endpoint"]
    FX_DITTO_URL          = local.dependency["ditto/endpoint"]
    TEMPORAL_ADDRESS      = module.temporal.endpoint
    TEMPORAL_NAMESPACE    = module.temporal.namespace
  }
  temporal_key = { TEMPORAL_API_KEY = module.secrets.arns["temporal-api-key"] }
  ditto_key    = { FX_DITTO_KEY = local.dependency["ditto/clients/corridor/key-arn"] }
  # Each process gets only the secrets its command requires.
  secrets = {
    api = merge(local.passwords, local.temporal_key, local.ditto_key, {
      FX_WEBHOOK_SECRET     = local.dependency["ditto/clients/corridor/webhook-secret-arn"]
      FX_AUDITOR_KEY        = module.secrets.arns["auditor-key"]
      FX_OPERATOR_KEY_ALICE = module.secrets.arns["operator-key-alice"]
      FX_OPERATOR_KEY_BOB   = module.secrets.arns["operator-key-bob"]
    })
    worker = merge(local.temporal_key, local.ditto_key, {
      FX_APP_PASSWORD  = local.passwords.FX_APP_PASSWORD
      FX_LEDGER_SECRET = local.dependency["ledger/clients/corridor-worker/secret-arn"]
    })
    dispatch = merge(local.temporal_key, { FX_APP_PASSWORD = local.passwords.FX_APP_PASSWORD })
  }
}

module "secrets" {
  source    = "../../modules/secrets"
  prefix    = "/sandbox/corridor"
  generated = { for name in ["app-password", "audit-password", "auditor-key", "operator-key-alice", "operator-key-bob"] : name => "" }
  external  = ["temporal-api-key"]
}

module "database" {
  source     = "../../modules/database"
  name       = "corridor-sandbox"
  database   = "fx"
  vpc_id     = local.foundation["vpc-id"]
  subnet_ids = local.private_subnets
}

module "temporal" {
  source = "../../modules/temporal"
  name   = "corridor-sandbox"
}

module "edge" {
  source            = "../../modules/edge"
  name              = "corridor-sandbox"
  vpc_id            = local.foundation["vpc-id"]
  public_subnet_ids = split(",", local.foundation["public-subnet-ids"])
  certificate_arn   = var.certificate_arn
}

# Migrations connect as the master user, whose password RDS keeps in Secrets Manager; pgx reads
# PGPASSWORD when the URL carries none. The run also sets the roles' passwords.
module "migrate" {
  source        = "../../modules/service"
  name          = "fx-migrate"
  cluster_arn   = local.foundation["cluster-arn"]
  vpc_id        = local.foundation["vpc-id"]
  subnet_ids    = local.private_subnets
  image         = var.image
  command       = ["migrate"]
  desired_count = 0
  environment   = { FX_DATABASE_URL = format(local.database, "postgres") }
  secrets       = merge(local.passwords, { PGPASSWORD = "${module.database.master_secret_arn}:password::" })
}

module "api" {
  source           = "../../modules/service"
  name             = "fx-api"
  cluster_arn      = local.foundation["cluster-arn"]
  vpc_id           = local.foundation["vpc-id"]
  subnet_ids       = local.private_subnets
  namespace_id     = local.foundation["namespace-id"]
  image            = var.image
  command          = ["api"]
  ports            = [8080, 8081]
  ingress_cidrs    = [local.foundation["vpc-cidr"]]
  target_group_arn = module.edge.target_group_arn
  health_command   = ["CMD", "/fx", "healthcheck"]
  environment      = local.environment
  secrets          = local.secrets.api
}

module "worker" {
  source        = "../../modules/service"
  name          = "fx-worker"
  cluster_arn   = local.foundation["cluster-arn"]
  vpc_id        = local.foundation["vpc-id"]
  subnet_ids    = local.private_subnets
  image         = var.image
  command       = ["worker"]
  desired_count = 2
  environment   = local.environment
  secrets       = local.secrets.worker
}

module "dispatch" {
  source      = "../../modules/service"
  name        = "fx-dispatch"
  cluster_arn = local.foundation["cluster-arn"]
  vpc_id      = local.foundation["vpc-id"]
  subnet_ids  = local.private_subnets
  image       = var.image
  command     = ["dispatch"]
  environment = local.environment
  secrets     = local.secrets.dispatch
}

resource "aws_vpc_security_group_ingress_rule" "database" {
  for_each                     = { for service in [module.migrate, module.api, module.worker, module.dispatch] : service.name => service.security_group_id }
  security_group_id            = module.database.security_group_id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

module "deploy_role" {
  source            = "../../modules/deploy-role"
  repository        = "xJohnNascimento/corridor"
  oidc_provider_arn = local.foundation["oidc-provider-arn"]
  cluster_arn       = local.foundation["cluster-arn"]
  families          = ["fx-migrate", "fx-api", "fx-worker", "fx-dispatch"]
  pass_role_arns = flatten([
    for service in [module.migrate, module.api, module.worker, module.dispatch] : [service.execution_role_arn, service.task_role_arn]
  ])
}

output "api_dns_name" {
  value = module.edge.dns_name
}

output "deploy_role_arn" {
  value = module.deploy_role.arn
}
