variable "y" {
  type = string
}

locals {
  greeting = "hello ${var.y}"
}
