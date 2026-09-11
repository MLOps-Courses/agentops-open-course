#!/usr/bin/env bash

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/lib.sh"

require_cmd kubectl platform
require_cmd yq gateway

infra_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

for overlay in local gke; do
	manifest="${tmp_dir}/${overlay}.yaml"
	if [[ "${overlay}" == "gke" ]]; then
		GCP_PROJECT_ID=agentops-course-check \
			GKE_CLUSTER_DNS_IP=10.30.0.10 \
			"${infra_dir}/scripts/render-gke.sh" >"${manifest}"
	else
		kubectl kustomize "${infra_dir}/k8s/overlays/${overlay}" >"${manifest}"
	fi

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
	mcp_state_read_only="$(
		yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp")
      | .spec.template.spec.containers[]
      | select(.name == "mcp")
      | .volumeMounts[]
      | select(.name == "state")
      | .readOnly' "${manifest}"
	)"
	gateway_automount="$(
		yq -r 'select(.kind == "ServiceAccount" and .metadata.name == "agentgateway")
      | .automountServiceAccountToken' "${manifest}"
	)"
	tempo_automount="$(
		yq -r 'select(.kind == "ServiceAccount" and .metadata.name == "tempo")
      | .automountServiceAccountToken' "${manifest}"
	)"
	tempo_claim="$(
		yq -r 'select(.kind == "Deployment" and .metadata.name == "tempo")
      | .spec.template.spec.volumes[]
      | select(.name == "data")
      | .persistentVolumeClaim.claimName' "${manifest}"
	)"
	tempo_data_mount="$(
		yq -r 'select(.kind == "Deployment" and .metadata.name == "tempo")
      | .spec.template.spec.containers[]
      | select(.name == "tempo")
      | .volumeMounts[]
      | select(.name == "data")
      | .mountPath' "${manifest}"
	)"
	tempo_block_path="$(
		yq -r 'select(.kind == "ConfigMap" and (.metadata.name | test("^tempo-config")))
      | .data."config.yaml"' "${manifest}" |
			yq -r '.storage.trace.local.path'
	)"
	unbounded_tmp_volumes="$(
		yq -r 'select(
        .spec.template.spec.volumes != null
        and ([.spec.template.spec.volumes[]
          | select(.name == "tmp" and .emptyDir.sizeLimit == null)] | length > 0)
      )
      | .metadata.name' "${manifest}"
	)"

	assert_eq "${overlay} agent state claim" "${agent_claim}" "agentops-agent-state"
	assert_eq "${overlay} agent tmp limit" "${agent_tmp_limit}" "128Mi"
	assert_eq "${overlay} MCP shared state claim" "${mcp_claim}" "${agent_claim}"
	assert_eq "${overlay} MCP fsGroup" "${mcp_fs_group}" "10001"
	assert_eq "${overlay} MCP state read-only" "${mcp_state_read_only}" "true"
	assert_eq "${overlay} gateway token automount" "${gateway_automount}" "false"
	assert_eq "${overlay} Tempo token automount" "${tempo_automount}" "false"
	assert_eq "${overlay} Tempo trace claim" "${tempo_claim}" "tempo"
	assert_eq "${overlay} Tempo data mount" "${tempo_data_mount}" "/var/tempo"
	# Both overlays keep trace blocks on the claim: Tempo's only storage destination
	# here is that PersistentVolumeClaim, so local and GKE cannot diverge into one
	# cluster writing traces to a bucket and the other to a disk.
	assert_eq "${overlay} Tempo block path" "${tempo_block_path}" "/var/tempo/blocks"
	[[ -z "${unbounded_tmp_volumes}" ]]
done
