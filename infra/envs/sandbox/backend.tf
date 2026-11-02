terraform {
  backend "s3" {
    key          = "corridor/sandbox.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}
