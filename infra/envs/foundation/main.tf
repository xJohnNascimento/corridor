terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "corridor", Environment = "sandbox", ManagedBy = "terraform" }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

module "network" {
  source = "../../modules/network"
  name   = "sandbox"
}

module "cluster" {
  source = "../../modules/cluster"
  name   = "sandbox"
  vpc_id = module.network.vpc_id
}

module "github_oidc" {
  source = "../../modules/github-oidc"
}

# SSM parameters are the only contract with the ledger's and Ditto's repositories: no repository
# reads another's state.
locals {
  parameters = {
    "vpc-id"             = module.network.vpc_id
    "vpc-cidr"           = module.network.vpc_cidr
    "private-subnet-ids" = join(",", module.network.private_subnet_ids)
    "public-subnet-ids"  = join(",", module.network.public_subnet_ids)
    "cluster-arn"        = module.cluster.cluster_arn
    "namespace-id"       = module.cluster.namespace_id
    "namespace-name"     = module.cluster.namespace_name
    "oidc-provider-arn"  = module.github_oidc.provider_arn
  }
}

resource "aws_ssm_parameter" "foundation" {
  for_each = local.parameters
  name     = "/sandbox/foundation/${each.key}"
  type     = "String"
  value    = each.value
}

output "parameters" {
  value = keys(local.parameters)
}
