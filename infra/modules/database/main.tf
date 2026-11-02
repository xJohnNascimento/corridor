terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

variable "name" {
  type = string
}

variable "database" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "subnet_ids" {
  type = list(string)
}

resource "aws_db_subnet_group" "this" {
  name       = var.name
  subnet_ids = var.subnet_ids
}

# Callers add one ingress rule per client service's security group.
resource "aws_security_group" "this" {
  name   = "${var.name}-database"
  vpc_id = var.vpc_id
}

# A minimum of zero lets an idle cluster pause; a service that polls keeps it at its floor instead
# (see the cost table in infra/README.md).
# RDS manages the master password in Secrets Manager, so it never appears in Terraform state.
resource "aws_rds_cluster" "this" {
  cluster_identifier          = var.name
  engine                      = "aurora-postgresql"
  engine_version              = "17.4"
  database_name               = var.database
  master_username             = "postgres"
  manage_master_user_password = true
  db_subnet_group_name        = aws_db_subnet_group.this.name
  vpc_security_group_ids      = [aws_security_group.this.id]
  storage_encrypted           = true
  # The sandbox is destroyed after each evidence run and holds only synthetic data.
  skip_final_snapshot = true
  serverlessv2_scaling_configuration {
    min_capacity             = 0
    max_capacity             = 2
    seconds_until_auto_pause = 600
  }
}

resource "aws_rds_cluster_instance" "this" {
  identifier         = "${var.name}-1"
  cluster_identifier = aws_rds_cluster.this.id
  instance_class     = "db.serverless"
  engine             = aws_rds_cluster.this.engine
  engine_version     = aws_rds_cluster.this.engine_version
}

output "endpoint" {
  value = aws_rds_cluster.this.endpoint
}

output "database" {
  value = aws_rds_cluster.this.database_name
}

output "security_group_id" {
  value = aws_security_group.this.id
}

output "master_secret_arn" {
  value = aws_rds_cluster.this.master_user_secret[0].secret_arn
}
