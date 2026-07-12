#!/usr/bin/env bash

set -Eeuo pipefail

mkdir -p .agents/tmp
tmp_dir=$(mktemp -d .agents/tmp/infra-check.XXXXXX)

# The secured host profile references demo TLS/JWT material that stays
# gitignored. Generate it on demand for validation, but remove it again when
# this script created it: `mise run secure` rightly flags private keys in the
# tree, and a learner who never ran Chapter 5.5 must keep a clean scan.
gateway_auth_dir="infra/agentgateway/host/auth"
cleanup_gateway_auth=0
if [[ ! -d "${gateway_auth_dir}" ]]; then
	cleanup_gateway_auth=1
fi
trap 'rm -rf "${tmp_dir}"; [[ "${cleanup_gateway_auth}" == "0" ]] || rm -rf "${gateway_auth_dir}"' EXIT

for overlay in local gke; do
	rendered="${tmp_dir}/${overlay}.yaml"
	kustomize build "infra/k8s/overlays/${overlay}" >"${rendered}"
	kubeconform -strict -ignore-missing-schemas -summary "${rendered}"
	kube-linter lint --fail-if-no-objects-found --with-color=false "${rendered}"
done

# SOPS guard rail (Ch. 6.5): every manifest under infra/**/secrets/ must be
# ciphertext — sops metadata present and each data/stringData value ENC[...] —
# so a plaintext Secret never lands in git through the secrets path. The
# gitignored infra/secrets/ directory holds the local age key, not manifests.
secret_manifests="$(find infra -path infra/secrets -prune -o -path '*/secrets/*' \( -name '*.yaml' -o -name '*.yml' \) ! -name '*.dec.yaml' -print | sort)"
while IFS= read -r secret_manifest; do
	[[ -n "${secret_manifest}" ]] || continue
	if ! yq -e '(.sops | length > 0) and ([.data // {}, .stringData // {}] | map(to_entries[].value) | flatten | all_c(test("^ENC\[")))' "${secret_manifest}" >/dev/null 2>&1; then
		echo "plaintext Secret in an infra secrets path (encrypt with infra/scripts/secrets.sh): ${secret_manifest}" >&2
		exit 1
	fi
done <<<"${secret_manifests}"

infra/scripts/check-state.sh

infra/scripts/gateway-tls.sh
infra/scripts/gateway-jwt.sh >/dev/null
openssl verify \
	-CAfile "${gateway_auth_dir}/ca-cert.pem" \
	"${gateway_auth_dir}/tls-cert.pem"
openssl x509 \
	-in "${gateway_auth_dir}/tls-cert.pem" \
	-checkhost localhost \
	-noout

for gateway_config in infra/agentgateway/host/config.yaml infra/agentgateway/host/config-auth.yaml infra/agentgateway/k3d/config.yaml infra/agentgateway/gke/config.yaml; do
	agentgateway --validate-only -f "${gateway_config}"
done

# The browser client is served from one fixed loopback origin. Keep every
# port-forwardable A2A profile usable without opening CORS to arbitrary sites.
for gateway_config in infra/agentgateway/host/config.yaml infra/agentgateway/k3d/config.yaml infra/agentgateway/gke/config.yaml; do
	cors='.binds[] | select(.port == 3001) | .listeners[].routes[].policies.cors'
	cors_origins=$(yq -r "${cors} | .allowOrigins | join(\",\")" "${gateway_config}")
	cors_methods=$(yq -r "${cors} | .allowMethods | join(\",\")" "${gateway_config}")
	cors_headers=$(yq -r "${cors} | .allowHeaders | join(\",\")" "${gateway_config}")
	[[ "${cors_origins}" == "http://localhost:8001" ]]
	[[ "${cors_methods}" == "GET,POST,OPTIONS" ]]
	[[ "${cors_headers}" == "content-type" ]]
done

docker compose -f infra/observability/compose.yaml config --quiet
(cd infra && skaffold diagnose --yaml-only -f skaffold.yaml -p local) >"${tmp_dir}/skaffold-local.yaml"
(cd infra && skaffold diagnose --yaml-only -f skaffold.yaml -p gke) >"${tmp_dir}/skaffold-gke.yaml"

rendered="${tmp_dir}/skaffold-render.yaml"
(
	cd infra
	skaffold render \
		--filename skaffold.yaml \
		--profile local \
		--offline \
		--digest-source tag \
		--images agentops-agent=agentops-agent:infra-check \
		--images agentops-mlflow=agentops-mlflow:infra-check
) >"${rendered}"
agent_image="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.image' "${rendered}")"
mcp_image="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp") | .spec.template.spec.containers[] | select(.name == "mcp") | .image' "${rendered}")"
mlflow_image="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "mlflow") | .spec.template.spec.containers[] | select(.name == "mlflow") | .image' "${rendered}")"
[[ "${agent_image}" == "${mcp_image}" ]]
[[ "${agent_image##*/}" == "agentops-agent:infra-check" ]]
[[ "${mlflow_image##*/}" == "agentops-mlflow:infra-check" ]]

# A TCP socket can be open while the dataset/session store is unusable. Assert
# the rendered MCP workload keeps the real HTTP probe and drain contract from
# issue #2 instead of relying only on schema validation.
mcp_grace="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp") | .spec.template.spec.terminationGracePeriodSeconds' "${rendered}")"
mcp_startup="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp") | .spec.template.spec.containers[] | select(.name == "mcp") | .startupProbe.httpGet.path' "${rendered}")"
mcp_readiness="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp") | .spec.template.spec.containers[] | select(.name == "mcp") | .readinessProbe.httpGet.path' "${rendered}")"
mcp_liveness="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp") | .spec.template.spec.containers[] | select(.name == "mcp") | .livenessProbe.httpGet.path' "${rendered}")"
[[ "${mcp_grace}" -gt 10 ]]
[[ "${mcp_startup}" == "/livez" ]]
[[ "${mcp_readiness}" == "/healthz" ]]
[[ "${mcp_liveness}" == "/livez" ]]

helmfile --file infra/helmfile.yaml --quiet lint --args '--quiet'

uv lock --directory infra/mlflow --check

tofu -chdir=infra/gcp fmt -check -recursive
tofu -chdir=infra/gcp init -backend=false -input=false -lockfile=readonly
tofu -chdir=infra/gcp validate
tflint --chdir=infra/gcp --minimum-failure-severity=warning
