resource "google_service_account" "nodes" {
  project      = var.project_id
  account_id   = "agentops-gke-nodes"
  display_name = "AgentOps GKE nodes"

  depends_on = [google_project_service.required]
}

resource "google_service_account" "agentgateway" {
  project      = var.project_id
  account_id   = "agentgateway"
  display_name = "AgentOps agentgateway"

  depends_on = [google_project_service.required]
}

resource "google_service_account" "mlflow" {
  project      = var.project_id
  account_id   = "mlflow"
  display_name = "AgentOps MLflow"

  depends_on = [google_project_service.required]
}

resource "google_project_iam_member" "nodes_default" {
  project = var.project_id
  role    = "roles/container.defaultNodeServiceAccount"
  member  = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_project_iam_member" "nodes_registry" {
  project = var.project_id
  role    = "roles/artifactregistry.reader"
  member  = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_project_iam_member" "agentgateway_vertex" {
  project = var.project_id
  role    = "roles/aiplatform.user"
  member  = "serviceAccount:${google_service_account.agentgateway.email}"
}

resource "google_project_iam_member" "agentgateway_service_usage" {
  project = var.project_id
  role    = "roles/serviceusage.serviceUsageConsumer"
  member  = "serviceAccount:${google_service_account.agentgateway.email}"
}

resource "google_storage_bucket_iam_member" "mlflow_objects" {
  bucket = google_storage_bucket.mlflow.name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${google_service_account.mlflow.email}"
}

resource "google_service_account_iam_member" "agentgateway_workload_identity" {
  service_account_id = google_service_account.agentgateway.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[agentops/agentgateway]"
}

resource "google_service_account_iam_member" "mlflow_workload_identity" {
  service_account_id = google_service_account.mlflow.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[agentops/mlflow]"
}
