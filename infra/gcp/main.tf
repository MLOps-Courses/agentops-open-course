locals {
  mlflow_bucket_name = coalesce(var.mlflow_bucket_name, "${var.project_id}-mlflow-artifacts")
  labels = {
    app        = "agentops-open-course"
    managed_by = "opentofu"
  }

  required_services = toset([
    "aiplatform.googleapis.com",
    "artifactregistry.googleapis.com",
    "compute.googleapis.com",
    "container.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "serviceusage.googleapis.com",
    "sts.googleapis.com",
    "storage.googleapis.com",
  ])
}

resource "google_project_service" "required" {
  for_each = local.required_services

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

resource "google_compute_network" "agentops" {
  name                    = "agentops"
  project                 = var.project_id
  auto_create_subnetworks = false

  depends_on = [google_project_service.required]
}

resource "google_compute_subnetwork" "agentops" {
  name          = "agentops-${var.region}"
  project       = var.project_id
  region        = var.region
  network       = google_compute_network.agentops.id
  ip_cidr_range = var.subnet_cidr

  secondary_ip_range {
    range_name    = "agentops-pods"
    ip_cidr_range = var.pods_cidr
  }

  secondary_ip_range {
    range_name    = "agentops-services"
    ip_cidr_range = var.services_cidr
  }
}

resource "google_artifact_registry_repository" "agentops" {
  project       = var.project_id
  location      = var.region
  repository_id = var.artifact_repository
  description   = "AgentOps Open Course container images"
  format        = "DOCKER"
  labels        = local.labels

  cleanup_policy_dry_run = false

  cleanup_policies {
    id     = "delete-images-after-thirty-days"
    action = "DELETE"

    condition {
      tag_state  = "ANY"
      older_than = "2592000s"
    }
  }

  cleanup_policies {
    id     = "keep-five-most-recent"
    action = "KEEP"

    most_recent_versions {
      keep_count = 5
    }
  }

  depends_on = [google_project_service.required]
}

resource "google_storage_bucket" "mlflow" {
  name                        = local.mlflow_bucket_name
  project                     = var.project_id
  location                    = upper(var.region)
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  labels                      = local.labels

  versioning {
    enabled = false
  }

  soft_delete_policy {
    retention_duration_seconds = 0
  }

  depends_on = [google_project_service.required]
}

resource "google_container_cluster" "agentops" {
  name     = var.cluster_name
  project  = var.project_id
  location = var.zone

  network    = google_compute_network.agentops.id
  subnetwork = google_compute_subnetwork.agentops.id

  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = var.deletion_protection

  networking_mode = "VPC_NATIVE"

  ip_allocation_policy {
    cluster_secondary_range_name  = "agentops-pods"
    services_secondary_range_name = "agentops-services"
  }

  release_channel {
    channel = "REGULAR"
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  network_policy {
    enabled  = true
    provider = "CALICO"
  }

  addons_config {
    network_policy_config {
      disabled = false
    }
  }

  master_auth {
    client_certificate_config {
      issue_client_certificate = false
    }
  }

  master_authorized_networks_config {
    dynamic "cidr_blocks" {
      for_each = var.master_authorized_networks

      content {
        cidr_block   = cidr_blocks.value.cidr_block
        display_name = cidr_blocks.value.display_name
      }
    }
  }

  # Public node IPs avoid a chargeable Cloud NAT. No workload is exposed by a
  # public Service; operators use authenticated kubectl port-forward instead.
  private_cluster_config {
    enable_private_nodes    = false
    enable_private_endpoint = false
  }

  logging_service    = "none"
  monitoring_service = "none"

  resource_labels = local.labels

  depends_on = [google_project_service.required]
}

resource "google_container_node_pool" "spot" {
  name     = "spot"
  project  = var.project_id
  location = var.zone
  cluster  = google_container_cluster.agentops.name

  node_count = 1

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  upgrade_settings {
    max_surge       = 0
    max_unavailable = 1
  }

  node_config {
    machine_type = var.machine_type
    spot         = var.spot_nodes
    disk_type    = "pd-standard"
    disk_size_gb = var.node_disk_size_gb
    image_type   = "COS_CONTAINERD"

    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    labels = {
      workload = "agentops"
    }

    metadata = {
      disable-legacy-endpoints = "true"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}
