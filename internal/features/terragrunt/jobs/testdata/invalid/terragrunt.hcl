terraform {
  source = "../modules/vpc"
  sources = "typo"
}

skip = true

remote_state {
  backend = "s3"
  bucket  = "wrong-place"
}

dependency "vpc" {
  config_path = "../vpc"
  outputs     = {}
}

locals {
  fine = true
}

terragrunt_cloud {
  enabled = true
}
