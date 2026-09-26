locals {
  a = templatefile("${path.module}/templates/a.tftpl", {})
  b = file("files/data.json")
  c = file("${path.module}/templates/missing.tftpl")
  d = templatefile("${path.module}/templates/", {})
  e = fileset(path.module, "*")
  f = file(local.p)
  p = "${path.module}/files/data.json"
}
