variable "number" {
  type = number
}

variable "name" {
  type = string
}

variable "unused" {
  type    = string
  default = "u"
}

locals {
  label = "${var.name}-${var.number}"
}

output "doubled" {
  description = "Twice the number."
  value       = var.number * 2
}

output "label" {
  value = local.label
}

resource "terraform_data" "copies" {
  count = var.number > 5 ? 2 : 1
  input = count.index
}
