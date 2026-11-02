terraform {
  backend "s3" {
    key          = "ditto/sandbox.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}
