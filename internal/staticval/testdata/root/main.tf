resource "random_pet" "p" {
  prefix = local.prefix
}

resource "terraform_data" "svc" {
  for_each = var.network.zones == [] ? {} : { web = 443, db = 5432 }
  input    = "${each.key}:${each.value}"
}

resource "terraform_data" "zone" {
  for_each = toset(var.network.zones)
  input    = each.value
}

resource "terraform_data" "worker" {
  count = 2
  input = count.index
}

data "local_file" "motd" {
  filename = "motd.txt"
}

module "child" {
  source = "./modules/child"
  number = 21
  name   = local.prefix
}

module "child2" {
  source = "./modules/child"
  number = 4
  name   = local.prefix
}

resource "terraform_data" "secret_each" {
  for_each = toset([var.secret])
  input    = each.value
}

module "child_secret" {
  source = "./modules/child"
  number = 1
  name   = var.secret
}

resource "local_sensitive_file" "f" {
  content  = local.prefix
  filename = local.upper
}

output "sensitive_prefix" {
  value     = local.upper
  sensitive = true
}

output "plain_prefix" {
  description = "The name prefix."
  value       = local.prefix
}
