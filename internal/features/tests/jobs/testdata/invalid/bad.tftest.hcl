test {
  parallel = true
}

mock_provider "random" {
  source = "./mocks"
}

variables {
  anything = "is fine"
}

run "one" {
  command = plan
  bogus   = 1

  assert {
    condition = true
  }

  plan_options {
    mode = refresh-only
    nope = true
  }

  module {
    version = "1.0"
  }

  unknown_block {}
}

run "two" "extra" {
}

run "three" {
  variables {
    a = 1
  }
  variables {
    b = 2
  }
}
