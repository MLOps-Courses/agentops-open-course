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

# The gateway's Vertex identity. Calling this role "narrow" would be wrong, and
# the honest description matters more than the reassuring one: roles/aiplatform.user
# grants project-wide use of Vertex AI, which is creation of billable resources —
# training jobs, endpoints, tuning, batch predictions — not only the one thing
# infra/agentgateway/gke/config.yaml actually does, which is generateContent
# against a publisher model. Anything executing in the agentgateway pod inherits
# it, and the GKE overlay deliberately allows that pod both the metadata server
# and 0.0.0.0/0:443 egress, so prompt injection reaching code execution reaches
# this role too.
#
# The lab accepts that because the alternative is a custom role, and a custom role
# is a per-project artifact a learner would have to create, name, and clean up
# before the GKE chapter runs at all. The spend ceiling is enforced one layer up
# instead — the llm route's request and token buckets in the gateway config — and
# the project itself is meant to be disposable. An operator hardening this for real
# should replace the binding with a custom role carrying aiplatform.endpoints.predict
# alone, which is the only permission the shipped route needs; do that with the
# gateway's own request log in hand, because adding a Vertex feature later fails
# closed and silently.
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

resource "google_service_account_iam_member" "agentgateway_workload_identity" {
  service_account_id = google_service_account.agentgateway.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[agentops/agentgateway]"

  # The workload pool becomes bindable only after GKE finishes creating it.
  depends_on = [google_container_cluster.agentops]
}
