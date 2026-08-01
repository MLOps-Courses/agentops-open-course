# Cheap GKE substrate

This OpenTofu module targets an existing billing-enabled project supplied through the required `project_id` variable. It creates a zonal GKE Standard cluster with one Spot `e2-standard-2` node, a VPC-native subnet, Artifact Registry, an MLflow GCS bucket, and separate Workload Identity service accounts for agentgateway and MLflow. It creates no Cloud NAT, Ingress, or public LoadBalancer.

Before planning, run `mise run install:gcp`, authenticate Application Default Credentials, run `GCP_PROJECT_ID=<project-id> mise run doctor:gcp` from the repository root, and set the same project plus your public `/32` in a gitignored `terraform.tfvars` based on `terraform.tfvars.example`. An approved disposable lab also snapshots the project's enabled APIs, service accounts, and IAM policy before planning:

```bash
set -euo pipefail

audit_dir="$(mktemp -d)"
project_id="<project-id>"
gcloud services list --enabled --project "${project_id}" \
  --format='value(config.name)' | sort -u >"${audit_dir}/services-before.txt"
gcloud iam service-accounts list --project "${project_id}" \
  --format='value(email)' | sort -u >"${audit_dir}/service-accounts-before.txt"
gcloud projects get-iam-policy "${project_id}" \
  --format=json >"${audit_dir}/project-iam-before.json"
test -z "$(tofu -chdir=infra/gcp state list)"

tofu -chdir=infra/gcp init
tofu -chdir=infra/gcp validate
tofu -chdir=infra/gcp plan -out=tfplan
```

Every inventory command must succeed: an authentication or authorization error is not an empty result. Before applying with empty state, also prove that the exact cluster, network, subnet, repository, bucket, and three service-account names in the plan do not already exist. Review the plan and current GCP prices before a later, explicitly approved `tofu -chdir=infra/gcp apply tfplan`. `../scripts/render-gke.sh` resolves the Workload Identity service accounts, MLflow bucket, and Vertex project from OpenTofu outputs; the committed manifests contain fail-visible placeholders instead of a project ID.

Spot VMs can stop at any time. The GKE overlay uses zonal standard persistent disks for the small PersistentVolumeClaims, while the GCS bucket preserves MLflow artifacts. [7.3. Costs](../../docs/7.%20Observability/7.3.%20Costs.md) owns the dated estimate and its assumptions; refresh every linked provider price immediately before applying.

## How do you isolate an approved deployment?

Keep the cloud lab out of the workstation's normal kubeconfig. After the approved apply, create an isolated file and run the credentials command printed by OpenTofu:

```bash
gke_kube_dir="$(mktemp -d)"
export KUBECONFIG="${gke_kube_dir}/config"
tofu -chdir=infra/gcp output -raw get_credentials_command
# Run the printed gcloud command while KUBECONFIG still points at this file.
```

Return to the repository root and run `mise run gke:deploy` only from one clean commit. The task requires that commit's full SHA, checks the exact GKE context again at each mutation, and configures Artifact Registry authentication in a temporary Docker config that it removes on exit.

Force both a model function-response turn and a read-only A2A retrieval. This command calls the billed Vertex model and belongs only inside the approved lab:

```bash
mise run gke:smoke
```

The command derives the expected context from OpenTofu, compares the exact HEAD-tagged Agent, generated workload, gateway image, and every live model owner with source, opens random loopback-only forwards, and closes them afterward. It passes only when the compatibility-pinned model returns the supplied synthetic tool result, the A2A task retrieves `INC-002`'s stable identity, and an exact counter delta proves `get_incident` ran once.

## How do you prove an approved lab was destroyed?

Return to the repository root, then capture the state-owned coordinates before destroying them. Every command below remains scoped to those exact outputs:

```bash
set -euo pipefail

: "${audit_dir:?reuse the protected audit directory created before apply}"
test -d "${audit_dir}"
project_id="$(tofu -chdir=infra/gcp output -raw project_id)"
cluster_name="$(tofu -chdir=infra/gcp output -raw cluster_name)"
cluster_zone="$(tofu -chdir=infra/gcp output -raw cluster_zone)"
region="$(tofu -chdir=infra/gcp output -raw region)"
network_name="$(tofu -chdir=infra/gcp output -raw network_name)"
subnetwork_name="$(tofu -chdir=infra/gcp output -raw subnetwork_name)"
bucket="$(tofu -chdir=infra/gcp output -raw mlflow_bucket_name)"
repository="$(tofu -chdir=infra/gcp output -raw artifact_registry_repository)"
repository_location="${repository%%-docker.pkg.dev/*}"
repository_name="${repository##*/}"
repository_resource="projects/${project_id}/locations/${repository_location}/repositories/${repository_name}"
agentgateway_sa="$(tofu -chdir=infra/gcp output -raw agentgateway_service_account)"
mlflow_sa="$(tofu -chdir=infra/gcp output -raw mlflow_service_account)"
node_sa="$(tofu -chdir=infra/gcp output -raw node_service_account)"
expected_context="gke_${project_id}_${cluster_zone}_${cluster_name}"
test "$(kubectl config current-context)" = "${expected_context}"
```

Capture every dynamically provisioned disk before deleting either namespace. GKE CSI disks are normally named `pvc-*`, so a `gke-agentops-*` name filter cannot prove their deletion:

```bash
./infra/scripts/gcp-lab-audit.sh \
  capture-pvs "${expected_context}" "${audit_dir}"
```

Delete the workload data first, then the controller and its course-owned namespace:

```bash
(
  cd infra
  SKAFFOLD_DEFAULT_REPO="${repository}" \
    skaffold delete \
    --filename skaffold.yaml \
    --profile gke \
    --kube-context "${expected_context}"
)
kubectl --context "${expected_context}" \
  wait --for=delete namespace/agentops --timeout=300s
helmfile \
  --file infra/helmfile.yaml \
  --kube-context "${expected_context}" \
  destroy
kubectl --context "${expected_context}" \
  delete namespace kagent --wait=true --timeout=300s
```

Wait until every PV name and every exact disk handle recorded in `pvs-before-delete.tsv` is absent. Parse the disk name from the last path segment of each handle and query it exactly with `gcloud compute disks list --project "${project_id}" --filter="name=<captured-name>"`. A failed inventory command fails cleanup; it is not evidence of absence.

`force_destroy=false` deliberately blocks OpenTofu while the exact MLflow bucket contains objects. List them first; delete them only when their loss is approved and the bucket name still equals the state output:

```bash
test "${bucket}" = "$(tofu -chdir=infra/gcp output -raw mlflow_bucket_name)"
gcloud storage ls --recursive "gs://${bucket}/**"
# Destructive, only after reviewing the exact object list:
gcloud storage rm --recursive "gs://${bucket}/**"
```

Create and apply a saved destroy plan instead of issuing an unreviewed destroy:

```bash
tofu -chdir=infra/gcp plan -destroy -out=destroy.tfplan
tofu -chdir=infra/gcp apply destroy.tfplan
test -z "$(tofu -chdir=infra/gcp state list)"
```

Finally, fail closed if an inventory cannot be read or still contains an exact state-owned resource:

```bash
set -euo pipefail

assert_empty() {
  local label="$1"
  shift
  local inventory
  inventory="$("$@")"
  if [[ -n "${inventory}" ]]; then
    printf '%s still exists:\n%s\n' "${label}" "${inventory}" >&2
    return 1
  fi
}

assert_empty "GKE cluster" \
  gcloud container clusters list \
  --project "${project_id}" \
  --filter="name=${cluster_name} AND location=${cluster_zone}" \
  --format='value(name)'
assert_empty "Artifact Registry repository" \
  gcloud artifacts repositories list \
  --project "${project_id}" \
  --location "${repository_location}" \
  --filter="name=\"${repository_resource}\"" \
  --format='value(name)'
assert_empty "MLflow bucket" \
  gcloud storage buckets list \
  --project "${project_id}" \
  --filter="name=${bucket}" \
  --format='value(name)'
assert_empty "VPC network" \
  gcloud compute networks list \
  --project "${project_id}" \
  --filter="name=${network_name}" \
  --format='value(name)'
assert_empty "VPC subnet" \
  gcloud compute networks subnets list \
  --project "${project_id}" \
  --regions "${region}" \
  --filter="name=${subnetwork_name}" \
  --format='value(name)'
assert_empty "GKE instances" \
  gcloud compute instances list \
  --project "${project_id}" \
  --filter="name~'^gke-${cluster_name}-'" \
  --format='value(name)'
assert_empty "GKE managed instance groups" \
  gcloud compute instance-groups managed list \
  --project "${project_id}" \
  --filter="name~'^gke-${cluster_name}-'" \
  --format='value(name)'
assert_empty "GKE instance templates" \
  gcloud compute instance-templates list \
  --project "${project_id}" \
  --filter="name~'^gke-${cluster_name}-'" \
  --format='value(name)'
assert_empty "VPC firewall rules" \
  gcloud compute firewall-rules list \
  --project "${project_id}" \
  --filter="network~'/networks/${network_name}$'" \
  --format='value(name)'
assert_empty "VPC routes" \
  gcloud compute routes list \
  --project "${project_id}" \
  --filter="network~'/networks/${network_name}$'" \
  --format='value(name)'
./infra/scripts/gcp-lab-audit.sh \
  verify-disks "${project_id}" "${audit_dir}/pvs-before-delete.tsv"
gcloud iam service-accounts list --project "${project_id}" \
  --format='value(email)' | sort -u >"${audit_dir}/service-accounts-after.txt"
gcloud projects get-iam-policy "${project_id}" \
  --format=json >"${audit_dir}/project-iam-after.json"
for service_account in "${node_sa}" "${agentgateway_sa}" "${mlflow_sa}"; do
  assert_empty "service account ${service_account}" \
    gcloud iam service-accounts list \
    --project "${project_id}" \
    --filter="email=${service_account}" \
    --format='value(email)'
  assert_empty "project IAM member ${service_account}" \
    jq -r \
    --arg member "serviceAccount:${service_account}" \
    '.bindings[]?.members[]? | select(. == $member)' \
    "${audit_dir}/project-iam-after.json"
done
```

The module deliberately leaves APIs enabled (`disable_on_destroy=false`) because it cannot know whether another project owner enabled them first. Restore only the exact set this lab added:

```bash
gcloud services list --enabled --project "${project_id}" \
  --format='value(config.name)' | sort -u >"${audit_dir}/services-after.txt"
comm -13 \
  "${audit_dir}/services-before.txt" \
  "${audit_dir}/services-after.txt" \
  >"${audit_dir}/services-new.txt"
```

Disable entries from `services-new.txt` without `--force`, in dependency-safe order: Vertex AI, GKE, Artifact Registry, IAM Credentials, Security Token Service, Cloud Storage, Compute Engine, IAM, then Service Usage last. Stop if Google reports a dependency. Regenerate `services-after.txt` and require it to equal `services-before.txt`.

Finally, compare the service-account and project-IAM inventories with their baseline. API activation can create Google-managed service agents; report any new provider-managed identity rather than deleting it automatically. Remove the isolated kubeconfig and audit directories only after OpenTofu state, exact cloud inventories, API comparison, and IAM review all pass.
