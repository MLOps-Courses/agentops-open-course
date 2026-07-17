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

	# Raw A2A is never a public workload port: only the in-namespace gateway
	# reaches the BYO pod. kagent's controller manages the CR/deployment through
	# the Kubernetes API and has no proven reason to bypass this data plane.
	a2a_ingress='.metadata.name == "agent-a2a-ingress" and .kind == "NetworkPolicy"'
	a2a_selector="$(yq -r "select(${a2a_ingress}) | .spec.podSelector.matchLabels.\"app.kubernetes.io/name\"" "${rendered}")"
	a2a_ingress_rules="$(yq -r "select(${a2a_ingress}) | .spec.ingress | length" "${rendered}")"
	a2a_sources="$(yq -r "select(${a2a_ingress}) | .spec.ingress[0].from | length" "${rendered}")"
	a2a_source_name="$(yq -r "select(${a2a_ingress}) | .spec.ingress[0].from[0].podSelector.matchLabels.\"app.kubernetes.io/name\"" "${rendered}")"
	a2a_source_namespace="$(yq -r "select(${a2a_ingress}) | .spec.ingress[0].from[0].namespaceSelector" "${rendered}")"
	a2a_ingress_port="$(yq -r "select(${a2a_ingress}) | .spec.ingress[0].ports[0].port" "${rendered}")"
	[[ "${a2a_selector}" == "agentops-agent" ]]
	[[ "${a2a_ingress_rules}" == "1" ]]
	[[ "${a2a_sources}" == "1" ]]
	[[ "${a2a_source_name}" == "agentgateway" ]]
	[[ "${a2a_source_namespace}" == "null" ]]
	[[ "${a2a_ingress_port}" == "8080" ]]

	agent_egress_selector="$(yq -r 'select(.kind == "NetworkPolicy" and .metadata.name == "agent-egress") | .spec.podSelector.matchLabels."app.kubernetes.io/name"' "${rendered}")"
	gateway_agent_target="$(yq -r 'select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-egress") | .spec.egress[] | select(.ports[].port == 8080) | .to[0].podSelector.matchLabels."app.kubernetes.io/name"' "${rendered}")"
	[[ "${agent_egress_selector}" == "agentops-agent" ]]
	[[ "${gateway_agent_target}" == "agentops-agent" ]]

	# Keep the public gateway surface explicit. The course namespace uses all
	# four listeners, while kagent needs only MCP and the model route.
	gateway_ingress='.kind == "NetworkPolicy" and .metadata.name == "agentgateway-ingress"'
	gateway_ingress_selector="$(yq -r "select(${gateway_ingress}) | .spec.podSelector.matchLabels.\"app.kubernetes.io/name\"" "${rendered}")"
	gateway_ingress_rules="$(yq -r "select(${gateway_ingress}) | .spec.ingress | length" "${rendered}")"
	gateway_ingress_sources="$(yq -r "select(${gateway_ingress}) | .spec.ingress[].from[].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\"" "${rendered}" | sort | paste -sd, -)"
	gateway_ingress_source_counts="$(yq -r "select(${gateway_ingress}) | .spec.ingress[].from | length" "${rendered}" | sort -n | paste -sd, -)"
	gateway_ingress_source_shapes="$(yq -r "select(${gateway_ingress}) | .spec.ingress[].from[] | keys | sort | join(\",\")" "${rendered}" | sort | paste -sd, -)"
	agentops_gateway_ports="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" == \"agentops\") | .ports[].port" "${rendered}" | sort -n | paste -sd, -)"
	agentops_gateway_protocols="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" == \"agentops\") | .ports[].protocol" "${rendered}" | sort | paste -sd, -)"
	kagent_gateway_ports="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" == \"kagent\") | .ports[].port" "${rendered}" | sort -n | paste -sd, -)"
	kagent_gateway_protocols="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" == \"kagent\") | .ports[].protocol" "${rendered}" | sort | paste -sd, -)"
	[[ "${gateway_ingress_selector}" == "agentgateway" ]]
	[[ "${gateway_ingress_rules}" == "2" ]]
	[[ "${gateway_ingress_sources}" == "agentops,kagent" ]]
	[[ "${gateway_ingress_source_counts}" == "1,1" ]]
	[[ "${gateway_ingress_source_shapes}" == "namespaceSelector,namespaceSelector" ]]
	[[ "${agentops_gateway_ports}" == "3000,3001,4000,15020" ]]
	[[ "${agentops_gateway_protocols}" == "TCP,TCP,TCP,TCP" ]]
	[[ "${kagent_gateway_ports}" == "3000,4000" ]]
	[[ "${kagent_gateway_protocols}" == "TCP,TCP" ]]

	agent_model="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_MODEL") | .value' "${rendered}")"
	agent_provider="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_MODEL_PROVIDER") | .value' "${rendered}")"
	agent_bind_host="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_A2A_BIND_HOST") | .value' "${rendered}")"
	retired_gateway_flag="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env | map(select(.name == "AGENT_GATEWAY_ENABLED")) | length' "${rendered}")"
	model_config="$(yq -r 'select(.kind == "ModelConfig" and .metadata.name == "agentgateway") | .spec.model' "${rendered}")"
	[[ "${agent_provider}" == "openai-compatible" ]]
	[[ "${agent_bind_host}" == "0.0.0.0" ]]
	[[ "${retired_gateway_flag}" == "0" ]]

	backup_state_read_only="$(yq -r 'select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") | .spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .volumeMounts[] | select(.name == "state") | (.readOnly // false)' "${rendered}")"
	backup_target_read_only="$(yq -r 'select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") | .spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .volumeMounts[] | select(.name == "backups") | (.readOnly // false)' "${rendered}")"
	[[ "${backup_state_read_only}" == "true" ]]
	[[ "${backup_target_read_only}" == "false" ]]

	if [[ "${overlay}" == "local" ]]; then
		[[ "${agent_model}" == "qwen3:4b-instruct" ]]
		[[ "${model_config}" == "qwen3:4b-instruct" ]]
	else
		[[ "${agent_model}" == "gemini-3.5-flash" ]]
		[[ "${model_config}" == "gemini-3.5-flash" ]]

		# Terraform selects Calico, not Dataplane V2. Lock both workloads to the
		# corresponding GKE metadata endpoint and reject the incompatible one.
		if grep -Fq "169.254.169.254" "${rendered}"; then
			echo "GKE overlay contains the Dataplane V2 metadata endpoint, but the cluster uses Calico" >&2
			exit 1
		fi
		wif_cidr="169.254.169.252/32"
		for policy in agentgateway-egress mlflow-egress; do
			wif_rule='select(.kind == "NetworkPolicy" and .metadata.name == "'"${policy}"'") | .spec.egress[] | select(.to[0].ipBlock.cidr == "'"${wif_cidr}"'")'
			wif_rule_count="$(yq -r "${wif_rule} | .to[0].ipBlock.cidr" "${rendered}" | awk 'NF { count++ } END { print count + 0 }')"
			wif_to_counts="$(yq -r "${wif_rule} | .to | length" "${rendered}" | sort -n | paste -sd, -)"
			wif_ports="$(yq -r "${wif_rule} | .ports[].port" "${rendered}" | sort -n | paste -sd, -)"
			wif_protocols="$(yq -r "${wif_rule} | .ports[].protocol" "${rendered}" | sort | paste -sd, -)"
			[[ "${wif_rule_count}" == "1" ]]
			[[ "${wif_to_counts}" == "1" ]]
			[[ "${wif_ports}" == "987,988" ]]
			[[ "${wif_protocols}" == "TCP,TCP" ]]
		done
		wif_cidr_count="$(grep -Fc "${wif_cidr}" "${rendered}")"
		[[ "${wif_cidr_count}" == "2" ]]
	fi
done

# Execute the rendered backup CronJob's exact inline program against throwaway
# SQLite state. A corrupt database must fail without publishing a visible
# snapshot; a later valid run must publish one marked snapshot while ignoring an
# unrelated hidden/incomplete directory.
backup_program="${tmp_dir}/state-backup.py"
yq -r '
	select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") |
	.spec.jobTemplate.spec.template.spec.containers[] |
	select(.name == "backup") |
	.args[0]
' "${tmp_dir}/local.yaml" >"${backup_program}"
backup_state="${tmp_dir}/backup-state"
backup_root="${tmp_dir}/backup-root"
mkdir -p "${backup_state}" "${backup_root}"
cp agents/data/incidents.db "${backup_state}/incidents.db"
printf 'not a SQLite database\n' >"${backup_state}/zz-corrupt.db"
if AGENT_STATE_DIR="${backup_state}" STATE_BACKUP_ROOT="${backup_root}" \
	agents/python/.venv/bin/python "${backup_program}" >"${tmp_dir}/backup-expected-failure.log" 2>&1; then
	echo "rendered backup program accepted a corrupt SQLite database" >&2
	exit 1
fi
leftover_backup_snapshot="$(find "${backup_root}" -mindepth 1 -maxdepth 1 -type d -print -quit)"
if [[ -n "${leftover_backup_snapshot}" ]]; then
	echo "rendered backup program published or retained a failed snapshot" >&2
	exit 1
fi
rm "${backup_state}/zz-corrupt.db"
mkdir "${backup_root}/.incomplete-check"
AGENT_STATE_DIR="${backup_state}" STATE_BACKUP_ROOT="${backup_root}" \
	agents/python/.venv/bin/python "${backup_program}" >"${tmp_dir}/backup-success.log"
[[ -d "${backup_root}/.incomplete-check" ]]
completed_backup_markers="$(find "${backup_root}" -mindepth 2 -maxdepth 2 -type f -name .complete | wc -l)"
visible_backup_dirs="$(find "${backup_root}" -mindepth 1 -maxdepth 1 -type d ! -name '.*' | wc -l)"
[[ "${completed_backup_markers}" -eq 1 ]]
[[ "${visible_backup_dirs}" -eq 1 ]]

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
grep -Fxq \
	'# SSL_CERT_FILE=../../infra/agentgateway/host/auth/ca-cert.pem' \
	.env.example
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

# The host file stays the canonical process-oriented profile. The Docker
# wrapper derives a network-correct copy without committing a second config.
host_container_config="${tmp_dir}/host-container.yaml"
infra/scripts/gateway-host.sh render >"${host_container_config}"
agentgateway --validate-only -f "${host_container_config}"
container_mcp="$(yq -r '.binds[] | select(.port == 3000) | .listeners[].routes[].backends[].mcp.targets[].mcp.host' "${host_container_config}")"
container_a2a="$(yq -r '.binds[] | select(.port == 3001) | .listeners[].routes[].backends[].host' "${host_container_config}")"
container_model="$(yq -r '.binds[] | select(.port == 4000) | .listeners[].routes[].backends[].ai.hostOverride' "${host_container_config}")"
container_stats_addr="$(yq -r '.config.statsAddr' "${host_container_config}")"
container_readiness_addr="$(yq -r '.config.readinessAddr' "${host_container_config}")"
container_admin_addr="$(yq -r '.config.adminAddr' "${host_container_config}")"
[[ "${container_mcp}" == "http://host.docker.internal:8000/mcp" ]]
[[ "${container_a2a}" == "host.docker.internal:8080" ]]
[[ "${container_model}" == "host.docker.internal:11434" ]]
[[ "${container_stats_addr}" == "0.0.0.0:15020" ]]
[[ "${container_readiness_addr}" == "0.0.0.0:15021" ]]
[[ "${container_admin_addr}" == "off" ]]

# Secured host mode uses the same container contract, but stages only the
# serving certificate/key and public JWKS into a private runtime directory.
host_auth_container_config="${tmp_dir}/host-auth-container.yaml"
AGENTOPS_GATEWAY_CONFIG=config-auth.yaml infra/scripts/gateway-host.sh render >"${host_auth_container_config}"
# Keep assertions on the untouched container render, but resolve its mounted
# auth paths to their generated host counterparts for local binary validation.
host_auth_validation_config="${tmp_dir}/host-auth-validation.yaml"
sed "s#/etc/agentgateway/auth/#${gateway_auth_dir}/#g" \
	"${host_auth_container_config}" >"${host_auth_validation_config}"
agentgateway --validate-only -f "${host_auth_validation_config}"
auth_certs="$(yq -r '.binds[].listeners[] | select(.tls != null) | .tls.cert' "${host_auth_container_config}" | sort -u)"
auth_keys="$(yq -r '.binds[].listeners[] | select(.tls != null) | .tls.key' "${host_auth_container_config}" | sort -u)"
auth_jwks="$(yq -r '.binds[].listeners[].routes[] | select(.policies.jwtAuth.jwks.file != null) | .policies.jwtAuth.jwks.file' "${host_auth_container_config}" | sort -u)"
auth_mcp="$(yq -r '.binds[] | select(.port == 3000) | .listeners[].routes[].backends[].mcp.targets[].mcp.host' "${host_auth_container_config}")"
auth_a2a="$(yq -r '.binds[] | select(.port == 3001) | .listeners[].routes[].backends[].host' "${host_auth_container_config}")"
auth_model="$(yq -r '.binds[] | select(.port == 4000) | .listeners[].routes[].backends[].ai.hostOverride' "${host_auth_container_config}")"
[[ "${auth_certs}" == "/etc/agentgateway/auth/tls-cert.pem" ]]
[[ "${auth_keys}" == "/etc/agentgateway/auth/tls-key.pem" ]]
[[ "${auth_jwks}" == "/etc/agentgateway/auth/jwks.json" ]]
[[ "${auth_mcp}" == "http://host.docker.internal:8000/mcp" ]]
[[ "${auth_a2a}" == "host.docker.internal:8080" ]]
[[ "${auth_model}" == "host.docker.internal:11434" ]]

# Inspect the actual argument array produced by the wrapper, rather than a
# parallel policy description that could drift from `docker run`.
host_container_args="${tmp_dir}/host-container.args"
infra/scripts/gateway-host.sh args >"${host_container_args}"
grep -Fxq -- "--user" "${host_container_args}"
container_user="$(awk '$0 == "--user" { getline; print; exit }' "${host_container_args}")"
container_cap_drop="$(awk '$0 == "--cap-drop" { getline; print; exit }' "${host_container_args}")"
container_security_opt="$(awk '$0 == "--security-opt" { getline; print; exit }' "${host_container_args}")"
container_tmpfs="$(awk '$0 == "--tmpfs" { getline; print; exit }' "${host_container_args}")"
[[ "${container_user}" == "65532:65532" ]]
grep -Fxq -- "--read-only" "${host_container_args}"
[[ "${container_cap_drop}" == "ALL" ]]
[[ "${container_security_opt}" == "no-new-privileges=true" ]]
[[ "${container_tmpfs}" == "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777" ]]
grep -Fxq -- "cr.agentgateway.dev/agentgateway:v1.3.1@sha256:c3ce7b75da90fef70239befcc1c3adc05152d7b9dd21fcb8351178026a2c4381" "${host_container_args}"

awk '$0 == "--publish" { getline; print }' "${host_container_args}" >"${tmp_dir}/host-container.published"
published_count="$(awk 'NF { count++ } END { print count + 0 }' "${tmp_dir}/host-container.published")"
[[ "${published_count}" == "5" ]]
grep -Fxq -- "127.0.0.1:3000:3000" "${tmp_dir}/host-container.published"
grep -Fxq -- "127.0.0.1:3001:3001" "${tmp_dir}/host-container.published"
grep -Fxq -- "127.0.0.1:4000:4000" "${tmp_dir}/host-container.published"
grep -Fxq -- "127.0.0.1:15020:15020" "${tmp_dir}/host-container.published"
grep -Fxq -- "127.0.0.1:15021:15021" "${tmp_dir}/host-container.published"
while IFS= read -r published_port; do
	[[ "${published_port}" == 127.0.0.1:* ]]
done <"${tmp_dir}/host-container.published"

host_auth_container_args="${tmp_dir}/host-auth-container.args"
AGENTOPS_GATEWAY_CONFIG=config-auth.yaml infra/scripts/gateway-host.sh args >"${host_auth_container_args}"
auth_mount="$(grep -F "dst=/etc/agentgateway/auth,readonly" "${host_auth_container_args}")"
[[ "${auth_mount}" == type=bind,src=*,dst=/etc/agentgateway/auth,readonly ]]

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
