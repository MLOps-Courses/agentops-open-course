output "project_id" {
  description = "Google Cloud project that owns the GKE lab."
  value       = var.project_id
}

output "cluster_name" {
  description = "Zonal GKE cluster name."
  value       = google_container_cluster.agentops.name
}

output "cluster_zone" {
  description = "GKE cluster zone."
  value       = google_container_cluster.agentops.location
}

output "region" {
  description = "GCP region containing the subnet, registry, and bucket."
  value       = var.region
}

output "network_name" {
  description = "Dedicated VPC network name."
  value       = google_compute_network.agentops.name
}

output "subnetwork_name" {
  description = "Dedicated regional subnet name."
  value       = google_compute_subnetwork.agentops.name
}

output "cluster_dns_ip" {
  description = "ClusterIP used by kube-dns inside the configured service CIDR."
  value       = cidrhost(var.services_cidr, 10)
}

output "get_credentials_command" {
  description = "Read-only command that adds this cluster to kubeconfig."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.agentops.name} --zone ${var.zone} --project ${var.project_id}"
}

output "artifact_registry_repository" {
  description = "Set this as SKAFFOLD_DEFAULT_REPO for the gke profile."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.agentops.repository_id}"
}

output "mlflow_bucket_name" {
  description = "GCS bucket used by the MLflow artifact proxy."
  value       = google_storage_bucket.mlflow.name
}

output "agentgateway_service_account" {
  description = "GSA impersonated by the agentgateway Kubernetes ServiceAccount."
  value       = google_service_account.agentgateway.email
}

output "mlflow_service_account" {
  description = "GSA impersonated by the MLflow Kubernetes ServiceAccount."
  value       = google_service_account.mlflow.email
}

output "node_service_account" {
  description = "Google service account used by the GKE node."
  value       = google_service_account.nodes.email
}
