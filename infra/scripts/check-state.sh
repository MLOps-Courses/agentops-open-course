#!/usr/bin/env bash
set -euo pipefail

infra_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

for overlay in local gke; do
	manifest="${tmp_dir}/${overlay}.yaml"
	kustomize build "${infra_dir}/k8s/overlays/${overlay}" >"${manifest}"

	agent_claim="$(
		yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent")
      | .spec.byo.deployment.volumes[]
      | select(.name == "state")
      | .persistentVolumeClaim.claimName' "${manifest}"
	)"
	agent_tmp_limit="$(
		yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent")
      | .spec.byo.deployment.volumes[]
      | select(.name == "tmp")
      | .emptyDir.sizeLimit' "${manifest}"
	)"
	mcp_claim="$(
		yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp")
      | .spec.template.spec.volumes[]
      | select(.name == "state")
      | .persistentVolumeClaim.claimName' "${manifest}"
	)"
	mcp_fs_group="$(
		yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp")
      | .spec.template.spec.securityContext.fsGroup' "${manifest}"
	)"
	mlflow_artifact_root="$(
		yq -r 'select(.kind == "Deployment" and .metadata.name == "mlflow")
      | .spec.template.spec.containers[]
      | select(.name == "mlflow")
      | .env[]
      | select(.name == "MLFLOW_DEFAULT_ARTIFACT_ROOT")
      | .value' "${manifest}"
	)"
	mlflow_artifacts_destination="$(
		yq -r 'select(.kind == "Deployment" and .metadata.name == "mlflow")
      | .spec.template.spec.containers[]
      | select(.name == "mlflow")
      | .env[]
      | select(.name == "MLFLOW_ARTIFACTS_DESTINATION")
      | .value' "${manifest}"
	)"
	unbounded_tmp_volumes="$(
		yq -r 'select(
        .spec.template.spec.volumes != null
        and ([.spec.template.spec.volumes[]
          | select(.name == "tmp" and .emptyDir.sizeLimit == null)] | length > 0)
      )
      | .metadata.name' "${manifest}"
	)"

	[[ "${agent_claim}" == "agentops-agent-state" ]]
	[[ "${agent_tmp_limit}" == "128Mi" ]]
	[[ "${mcp_claim}" == "${agent_claim}" ]]
	[[ "${mcp_fs_group}" == "10001" ]]
	[[ "${mlflow_artifact_root}" == "mlflow-artifacts:/" ]]
	if [[ "${overlay}" == "gke" ]]; then
		[[ "${mlflow_artifacts_destination}" == "gs://agentops-open-course-mlflow-artifacts" ]]
	else
		[[ "${mlflow_artifacts_destination}" == "/var/lib/mlflow/artifacts" ]]
	fi
	[[ -z "${unbounded_tmp_volumes}" ]]
done
