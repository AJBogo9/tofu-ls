run "child_module" {
  command = apply

  module {
    source = "./modules/child"
  }

  variables {
    name = "kid"
  }

  assert {
    condition     = output.greeting == "hello ${var.name}"
    error_message = "The child greets by name."
  }

  assert {
    condition     = terraform_data.x.input == "kid"
    error_message = "The child stores its name."
  }
}
