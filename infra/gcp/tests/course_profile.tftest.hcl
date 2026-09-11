# Plan-only tests use a mocked provider: they require no credentials, API calls,
# cloud resources, or spend. They pin the disposable lab's cost and network
# boundaries before an owner can approve a real saved plan.
mock_provider "google" {
  mock_resource "google_service_account" {
    defaults = {
      name  = "projects/test-project/serviceAccounts/mock-service-account@test-project.iam.gserviceaccount.com"
      email = "mock-service-account@test-project.iam.gserviceaccount.com"
    }
  }
}

variables {
  project_id = "test-project"
  master_authorized_networks = [{
    cidr_block   = "203.0.113.10/32"
    display_name = "approved-operator"
  }]
}

run "valid_disposable_profile" {
  command = plan

  assert {
    condition = (
      google_container_node_pool.spot.node_config[0].spot &&
      google_container_node_pool.spot.node_config[0].machine_type == "e2-standard-2" &&
      google_container_node_pool.spot.node_config[0].disk_size_gb == 30
    )
    error_message = "The disposable node must use the tested Spot e2-standard-2 profile with a 30 GB disk."
  }

  assert {
    condition     = !google_container_cluster.agentops.deletion_protection
    error_message = "The disposable cluster must remain deletable."
  }

  # The absent version floor is a decision, not an omission, so it is asserted:
  # a min_master_version added later would read like a pin, would not act as
  # one, and would rot out of the channel. main.tf carries the reasoning.
  assert {
    condition = (
      google_container_cluster.agentops.min_master_version == null &&
      google_container_cluster.agentops.release_channel[0].channel == "REGULAR" &&
      google_container_node_pool.spot.management[0].auto_upgrade
    )
    error_message = "The control plane must float on the REGULAR channel with no version floor, and the nodes must follow it."
  }

  assert {
    condition = (
      google_container_cluster.agentops.location == "europe-west1-b" &&
      google_compute_subnetwork.agentops.region == "europe-west1"
    )
    error_message = "The tested zone must remain inside the configured region."
  }

  assert {
    condition = (
      length(flatten([
        for config in google_container_cluster.agentops.master_authorized_networks_config : [
          for block in config.cidr_blocks : block.cidr_block
        ]
      ])) == 1 &&
      one(flatten([
        for config in google_container_cluster.agentops.master_authorized_networks_config : [
          for block in config.cidr_blocks : block.cidr_block
        ]
      ])) == "203.0.113.10/32"
    )
    error_message = "The public control plane must allow only the approved operator IPv4 /32."
  }
}

run "zone_must_match_region" {
  command = plan

  variables {
    zone = "us-central1-a"
  }

  expect_failures = [var.zone]
}

run "machine_type_cannot_amplify_cost" {
  command = plan

  variables {
    machine_type = "n2-standard-8"
  }

  expect_failures = [var.machine_type]
}

run "nodes_must_remain_spot" {
  command = plan

  variables {
    spot_nodes = false
  }

  expect_failures = [var.spot_nodes]
}

run "disk_cannot_exceed_tested_ceiling" {
  command = plan

  variables {
    node_disk_size_gb = 31
  }

  expect_failures = [var.node_disk_size_gb]
}

run "disk_cannot_be_too_small" {
  command = plan

  variables {
    node_disk_size_gb = 19
  }

  expect_failures = [var.node_disk_size_gb]
}

run "deletion_protection_cannot_block_teardown" {
  command = plan

  variables {
    deletion_protection = true
  }

  expect_failures = [var.deletion_protection]
}

run "control_plane_rejects_non_32" {
  command = plan

  variables {
    master_authorized_networks = [{
      cidr_block   = "203.0.113.0/24"
      display_name = "too-broad"
    }]
  }

  expect_failures = [var.master_authorized_networks]
}

run "control_plane_rejects_multiple_ranges" {
  command = plan

  variables {
    master_authorized_networks = [
      { cidr_block = "203.0.113.10/32", display_name = "operator-a" },
      { cidr_block = "198.51.100.20/32", display_name = "operator-b" },
    ]
  }

  expect_failures = [var.master_authorized_networks]
}

run "control_plane_cannot_overlap_vpc" {
  command = plan

  variables {
    master_authorized_networks = [{
      cidr_block   = "10.20.1.1/32"
      display_name = "private-address"
    }]
  }

  expect_failures = [var.master_authorized_networks]
}

run "pod_range_cannot_overlap_subnet" {
  command = plan

  variables {
    pods_cidr = "10.10.1.0/24"
  }

  expect_failures = [var.services_cidr]
}

run "service_range_cannot_overlap_subnet" {
  command = plan

  variables {
    services_cidr = "10.10.2.0/24"
  }

  expect_failures = [var.services_cidr]
}

run "service_range_cannot_overlap_pods" {
  command = plan

  variables {
    services_cidr = "10.20.2.0/24"
  }

  expect_failures = [var.services_cidr]
}
