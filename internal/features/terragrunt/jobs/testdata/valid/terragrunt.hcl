# Every block and attribute of a unit in Terragrunt 1.1.6.
include "root" {
  path           = find_in_parent_folders("root.hcl")
  expose         = true
  merge_strategy = "deep"
}

locals {
  region = "eu-north-1"
  tags   = { team = "platform" }
}

terraform {
  source                   = "../../modules//vpc"
  include_in_copy          = [".python-version"]
  exclude_from_copy        = ["*.bak"]
  copy_terraform_lock_file = false

  extra_arguments "vars" {
    commands           = get_terraform_commands_that_need_vars()
    arguments          = ["-lock-timeout=20m"]
    env_vars           = { TF_LOG = "INFO" }
    required_var_files = ["common.tfvars"]
    optional_var_files = ["${get_terragrunt_dir()}/optional.tfvars"]
  }

  before_hook "fmt" {
    commands     = ["plan"]
    execute      = ["tofu", "fmt"]
    working_dir  = get_terragrunt_dir()
    run_on_error = false
    if           = true
  }

  after_hook "done" {
    commands        = ["apply"]
    execute         = ["echo", "done"]
    suppress_stdout = true
  }

  error_hook "report" {
    commands  = ["apply"]
    execute   = ["echo", "failed"]
    on_errors = [".*"]
  }
}

remote_state {
  backend = "s3"
  config = {
    bucket = "state-${local.region}"
    key    = "${path_relative_to_include()}/tofu.tfstate"
  }
  generate = {
    path      = "backend.tf"
    if_exists = "overwrite_terragrunt"
  }
  encryption = {
    key_provider = "pbkdf2"
    passphrase   = get_env("PASSPHRASE")
  }
  disable_init                    = false
  disable_dependency_optimization = false
}

dependency "vpc" {
  config_path                             = "../vpc"
  enabled                                 = true
  skip_outputs                            = false
  mock_outputs                            = { vpc_id = "vpc-mock" }
  mock_outputs_allowed_terraform_commands = ["validate", "plan"]
  mock_outputs_merge_strategy_with_state  = "shallow"
}

dependency "subnets" {
  config_path = "../subnets"

  expansion {
    for_each = toset(["a", "b"])
  }
}

dependencies {
  paths = ["../vpc", "../rds"]
}

generate "provider" {
  path              = "provider.tf"
  if_exists         = "overwrite"
  if_disabled       = "remove"
  comment_prefix    = "# "
  disable_signature = false
  disable           = false
  hcl_fmt           = true
  contents          = <<EOT
provider "aws" {
  region = "${local.region}"
}
EOT
}

feature "fast" {
  default = false
}

exclude {
  if                   = feature.fast.value
  actions              = ["apply"]
  exclude_dependencies = false
  no_run               = true
}

errors {
  retry "transient" {
    retryable_errors   = get_default_retryable_errors()
    max_attempts       = 3
    sleep_interval_sec = 5
  }

  ignore "known" {
    ignorable_errors = [".*known.*", "!.*fatal.*"]
    message          = "known error"
    signals          = { safe = true }
  }
}

engine {
  source  = "github.com/gruntwork-io/terragrunt-engine-opentofu"
  version = "v0.0.16"
  type    = "rpc"
  meta    = { concurrent = true }
}

catalog {
  urls             = ["https://github.com/acme/catalog"]
  default_template = "templates/default"
  no_shell         = true
  no_hooks         = true
}

inputs = merge(local.tags, {
  vpc_id  = dependency.vpc.outputs.vpc_id
  subnets = [for s in dependency.subnets : s.outputs.id]
  root    = include.root.locals.account
})

download_dir                  = "/tmp/terragrunt"
prevent_destroy               = true
iam_role                      = "arn:aws:iam::123456789012:role/terragrunt"
iam_assume_role_duration      = 3600
iam_assume_role_session_name  = "terragrunt"
iam_web_identity_token        = get_env("TOKEN", "")
terraform_binary              = "tofu"
terraform_version_constraint  = ">= 1.6"
terragrunt_version_constraint = ">= 0.80"
