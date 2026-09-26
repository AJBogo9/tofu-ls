mock_provider "random" {
  mock_resource "random_pet" {
    defaults = {
      id = "mocked-pet"
    }
  }
}

override_resource {
  target = null_resource.n
  values = {
    id = "override-id"
  }
}

run "mocked_apply" {
  assert {
    condition     = random_pet.name.id == "mocked-pet"
    error_message = "The mock provider sets the pet name."
  }

  assert {
    condition     = null_resource.n.id == "override-id"
    error_message = "The override sets the null resource id."
  }
}
