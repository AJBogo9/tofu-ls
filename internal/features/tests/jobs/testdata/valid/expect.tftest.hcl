run "too_long" {
  command = plan

  variables {
    length = 9
  }

  expect_failures = [
    var.length,
  ]
}
