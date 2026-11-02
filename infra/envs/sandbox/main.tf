terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "ditto", Environment = "sandbox", ManagedBy = "terraform" }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "image" {
  type        = string
  description = "The Ditto image, pinned by digest"
}

data "aws_ssm_parameter" "foundation" {
  for_each = toset(["vpc-id", "vpc-cidr", "private-subnet-ids", "cluster-arn", "namespace-id", "namespace-name", "oidc-provider-arn"])
  name     = "/sandbox/foundation/${each.key}"
}

locals {
  foundation      = { for key, parameter in data.aws_ssm_parameter.foundation : key => nonsensitive(parameter.value) }
  private_subnets = split(",", local.foundation["private-subnet-ids"])
  # Ditto is a single synthetic service, so it connects as the master user.
  database = {
    environment = { DITTO_DATABASE_URL = "postgres://postgres@${module.database.endpoint}:5432/${module.database.database}?sslmode=require" }
    secrets     = { PGPASSWORD = "${module.database.master_secret_arn}:password::" }
  }
}

# Ditto verifies the keys and signs the webhooks, so it owns these secrets.
module "secrets" {
  source = "../../modules/secrets"
  prefix = "/sandbox/ditto"
  generated = {
    admin-key               = ""
    corridor-key            = ""
    corridor-webhook-secret = "whsec_"
  }
}

module "database" {
  source     = "../../modules/database"
  name       = "ditto-sandbox"
  database   = "ditto"
  vpc_id     = local.foundation["vpc-id"]
  subnet_ids = local.private_subnets
}

module "migrate" {
  source        = "../../modules/service"
  name          = "ditto-migrate"
  cluster_arn   = local.foundation["cluster-arn"]
  vpc_id        = local.foundation["vpc-id"]
  subnet_ids    = local.private_subnets
  image         = var.image
  command       = ["migrate"]
  desired_count = 0
  environment   = local.database.environment
  secrets       = local.database.secrets
}

module "ditto" {
  source         = "../../modules/service"
  name           = "ditto"
  cluster_arn    = local.foundation["cluster-arn"]
  vpc_id         = local.foundation["vpc-id"]
  subnet_ids     = local.private_subnets
  namespace_id   = local.foundation["namespace-id"]
  image          = var.image
  command        = ["serve"]
  ports          = [9002]
  ingress_cidrs  = [local.foundation["vpc-cidr"]]
  health_command = ["CMD", "/ditto", "healthcheck"]
  environment = merge(local.database.environment, {
    DITTO_PROFILE              = "sandbox"
    DITTO_DATASET_ID           = "sandbox"
    DITTO_WEBHOOK_URL_CORRIDOR = "http://fx-api.${local.foundation["namespace-name"]}:8081/webhooks/ditto"
  })
  secrets = merge(local.database.secrets, {
    DITTO_ADMIN_KEY               = module.secrets.arns["admin-key"]
    DITTO_KEY_CORRIDOR            = module.secrets.arns["corridor-key"]
    DITTO_WEBHOOK_SECRET_CORRIDOR = module.secrets.arns["corridor-webhook-secret"]
  })
}

resource "aws_vpc_security_group_ingress_rule" "database" {
  for_each                     = { for service in [module.migrate, module.ditto] : service.name => service.security_group_id }
  security_group_id            = module.database.security_group_id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

module "deploy_role" {
  source            = "../../modules/deploy-role"
  repository        = "xJohnNascimento/ditto"
  oidc_provider_arn = local.foundation["oidc-provider-arn"]
  cluster_arn       = local.foundation["cluster-arn"]
  families          = ["ditto-migrate", "ditto"]
  pass_role_arns = flatten([
    for service in [module.migrate, module.ditto] : [service.execution_role_arn, service.task_role_arn]
  ])
}

resource "aws_ssm_parameter" "published" {
  for_each = {
    "endpoint"                            = "http://ditto.${local.foundation["namespace-name"]}:9002"
    "clients/corridor/key-arn"            = module.secrets.arns["corridor-key"]
    "clients/corridor/webhook-secret-arn" = module.secrets.arns["corridor-webhook-secret"]
  }
  name  = "/sandbox/ditto/${each.key}"
  type  = "String"
  value = each.value
}

output "deploy_role_arn" {
  value = module.deploy_role.arn
}
