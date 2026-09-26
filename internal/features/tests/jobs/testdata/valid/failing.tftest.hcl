run "wrong_length" {
  command = plan

  variables {
    length = 3
  }

  assert {
    condition     = random_pet.name.length == 2
    error_message = "The pet name must have two words."
  }
}

run "after_failure" {
  command = plan
}
