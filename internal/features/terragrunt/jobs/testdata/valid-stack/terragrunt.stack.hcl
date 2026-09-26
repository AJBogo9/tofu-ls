include "shared" {
  path = "${get_repo_root()}/stacks/shared.stack.hcl"
}

locals {
  stage = "dev"
}

unit "vpc" {
  source                  = "../catalog/units/vpc"
  path                    = "vpc"
  values                  = { stage = local.stage }
  no_dot_terragrunt_stack = false
  no_validation           = false
  enabled                 = true

  autoinclude {
    dependency "db" {
      config_path  = unit.db.path
      mock_outputs = { arn = "arn:mock" }
    }

    inputs = {
      extra = true
    }
  }
}

unit "db" {
  source = "github.com/acme/catalog//units/db?ref=v1.0.0"
  path   = "db"
}

unit "shard" {
  source = "../catalog/units/shard"
  path   = "shard-${each.key}"

  expansion {
    for_each = toset(["a", "b"])
  }
}

stack "services" {
  source = "github.com/acme/catalog//stacks/services"
  path   = "services"
  values = { vpc_path = unit.vpc.path }

  autoinclude {
    unit "extra" {
      source = "../catalog/units/extra"
      path   = "extra"
    }
  }
}
