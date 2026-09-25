variable "x" {
  type    = string
  default = "a-default"
}

module "b" {
  source = "./b"
  y      = "${var.x}-via-a"
}
