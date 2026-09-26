variables {
  prefix = "unit"
}

run "plan_names" {
  command = plan

  assert {
    condition     = random_pet.name.prefix == var.prefix
    error_message = "The pet prefix must come from var.prefix."
  }

  assert {
    condition     = local.label == "unit-label"
    error_message = "The label is built from the prefix."
  }
}

run "apply_pet" {
  assert {
    condition     = output.pet != ""
    error_message = "The pet output is empty."
  }

  assert {
    condition     = terraform_data.echo.input == random_pet.name.id
    error_message = "The echo input must be the pet name."
  }
}

run "reuse_output" {
  command = plan

  variables {
    prefix = run.apply_pet.greeting
  }

  assert {
    condition     = module.child.greeting == "hello ${var.prefix}"
    error_message = "The child greets with the prefix."
  }
}
