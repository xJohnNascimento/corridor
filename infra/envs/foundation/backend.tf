terraform {
  backend "s3" {
    key          = "corridor/foundation.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}
