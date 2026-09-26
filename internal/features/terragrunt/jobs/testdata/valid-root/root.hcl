locals {
  account_vars = read_terragrunt_config(find_in_parent_folders("account.hcl"))
  account      = local.account_vars.locals.account_name
}

remote_state = {
  backend = "s3"
  config  = { bucket = "state-${local.account}" }
}

generate = {
  provider = {
    path      = "provider.tf"
    if_exists = "overwrite_terragrunt"
    contents  = "provider \"aws\" {}"
  }
}

inputs = {
  account = local.account
}
