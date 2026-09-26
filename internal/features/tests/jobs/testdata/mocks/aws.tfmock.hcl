mock_resource "aws_s3_bucket" {
  defaults = {
    arn = "arn:aws:s3:::test"
  }
}

mock_data "aws_region" {
  defaults = {
    name = "eu-north-1"
  }
}

override_resource {
  target = aws_s3_bucket.logs
}

run "not_here" {}
