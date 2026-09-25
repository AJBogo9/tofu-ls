locals {
  prefix   = format("%s-%s", var.project, var.stage)
  upper    = upper(local.prefix)
  subnets  = { for i, z in var.network.zones : z => cidrsubnet(var.network.cidr, var.network.subnet_bits, i) }
  pet      = "${local.prefix}-${random_pet.p.id}"
  started  = timestamp()
  yaml     = yamlencode({ a = 1 })
  hashed   = sha256(var.secret)
  missing  = "${var.required}-x"
  cycle_a  = local.cycle_b
  cycle_b  = local.cycle_a
  motd     = data.local_file.motd.content
  from_mod = module.child.doubled
  ws       = terraform.workspace
  file     = file("${path.module}/motd.txt")
}
