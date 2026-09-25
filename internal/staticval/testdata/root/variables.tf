variable "project" {
  description = "Project name."
  type        = string

  validation {
    condition     = length(var.project) > 2
    error_message = "Too short."
  }
}

variable "stage" {
  type    = string
  default = "dev"
}

variable "network" {
  type = object({
    cidr        = string
    subnet_bits = optional(number, 8)
    zones       = list(string)
  })
  default = {
    cidr  = "10.0.0.0/16"
    zones = ["a", "b"]
  }
}

variable "secret" {
  type      = string
  default   = "hunter2"
  sensitive = true
}

variable "required" {
  type = string
}

variable "flags" {
  type     = set(string)
  default  = ["x"]
  nullable = false
}
