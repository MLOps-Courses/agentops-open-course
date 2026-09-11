terraform {
  required_version = ">= 1.10.0, < 2.0.0"

  required_providers {
    google = {
      # `~> 7.42` admits every 7.x at or above 7.42 and refuses 8.x. The exact-patch
      # form it replaces (`~> 7.42.0`) admitted only 7.42.x, and upstream never shipped
      # a 7.42 patch — so `tofu init -upgrade` could never move it, and no Dependabot
      # ecosystem or freshness row watches this file. The 8.x line is deliberately out:
      # a major provider changes resource schemas, and this module's acceptance is
      # `tofu test` plus a plan against a real project, which nobody has re-run on 8.x.
      source  = "hashicorp/google"
      version = "~> 7.42"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
  zone    = var.zone
}
