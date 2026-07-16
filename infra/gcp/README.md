# Cheap GKE substrate

This OpenTofu module targets an existing billing-enabled project and defaults to `agentops-open-course`. It creates a zonal GKE Standard cluster with one Spot `e2-standard-2` node, a VPC-native subnet, Artifact Registry, an MLflow GCS bucket, and separate Workload Identity service accounts for agentgateway and MLflow. It creates no Cloud NAT, Ingress, or public LoadBalancer.

Before planning, authenticate Application Default Credentials, run `mise run doctor:gcp` from the repository root, and restrict the control plane to your public `/32` in a local `terraform.tfvars` based on `terraform.tfvars.example`.

```bash
tofu init
tofu validate
tofu plan -out=tfplan
```

Review the plan and current GCP prices before a later, explicitly approved `tofu apply tfplan`. After apply, `tofu output -raw get_credentials_command` prints the command that configures kubectl. The two values in `../k8s/overlays/gke/workload-identity.yaml`, the bucket patch in `../k8s/overlays/gke/kustomization.yaml`, and Vertex `projectId` in `../agentgateway/gke/config.yaml` use the default project; change those three declarative values when overriding `project_id`.

Spot VMs can stop at any time. The PersistentVolumeClaims preserve SQLite data across Pod replacement, while the GCS bucket preserves MLflow artifacts. Cloud prices and the GKE free-tier credit vary by billing account and region, so the under-$20 target is a design goal, not a billing guarantee.
