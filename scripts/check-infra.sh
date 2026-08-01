#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd yq gateway
require_cmd kubectl platform
require_cmd kubeconform platform
require_cmd kube-linter platform
require_cmd helmfile platform
require_cmd skaffold platform
require_cmd docker gateway
require_cmd tofu gcp
require_cmd tflint gcp

mkdir -p .agents/tmp
tmp_dir=$(mktemp -d .agents/tmp/infra-check.XXXXXX)
readonly kagent_schema_location='infra/kagent/schemas/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

# Rendering must never consult the maintainer's active cluster. Skaffold and
# Helmfile inspect KUBECONFIG even for offline renders, which can otherwise
# trigger cloud authentication or make the result depend on the current context.
export KUBECONFIG=/dev/null
export AGENT_SOURCE_COMMIT
AGENT_SOURCE_COMMIT="$(git rev-parse HEAD)"

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

# One source of truth for the alerting rules (Ch. 7.2): the Compose stack's file
# is a symlink to the overlay's, so both planes evaluate identical expressions.
# Assert the link itself — an editor or formatter that writes through it would
# silently restore two independently drifting copies.
[[ -L infra/observability/prometheus-rules.yml ]]
[[ ! -L infra/k8s/overlays/local/prometheus-rules.yaml ]]
prometheus_rules_link="$(readlink infra/observability/prometheus-rules.yml)"
[[ "${prometheus_rules_link}" == "../k8s/overlays/local/prometheus-rules.yaml" ]]

for overlay in local gke; do
	rendered="${tmp_dir}/${overlay}.yaml"
	if [[ ${overlay} == gke ]]; then
		GCP_PROJECT_ID=agentops-course-check \
			MLFLOW_BUCKET_NAME=agentops-course-check-mlflow \
			GKE_CLUSTER_DNS_IP=10.30.0.10 \
			infra/scripts/render-gke.sh >"${rendered}"
	else
		kubectl kustomize "infra/k8s/overlays/${overlay}" >"${rendered}"
	fi
	kubeconform \
		-strict \
		-kubernetes-version 1.36.0 \
		-schema-location default \
		-schema-location "${kagent_schema_location}" \
		-summary \
		"${rendered}"
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

	# Keep gateway ingress source-and-port specific. The BYO agent uses MCP and
	# the model route, the collector alone scrapes metrics, and only the kagent
	# controller crosses namespaces. No pod needs the A2A listener: learners
	# reach it through a temporary kubectl port-forward.
	gateway_ingress='.kind == "NetworkPolicy" and .metadata.name == "agentgateway-ingress"'
	gateway_ingress_selector="$(yq -r "select(${gateway_ingress}) | .spec.podSelector.matchLabels.\"app.kubernetes.io/name\"" "${rendered}")"
	gateway_ingress_rules="$(yq -r "select(${gateway_ingress}) | .spec.ingress | length" "${rendered}")"
	gateway_ingress_source_counts="$(yq -r "select(${gateway_ingress}) | .spec.ingress[].from | length" "${rendered}" | sort -n | paste -sd, -)"
	agent_gateway_ports="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].podSelector.matchLabels.\"app.kubernetes.io/name\" == \"agentops-agent\") | .ports[].port" "${rendered}" | sort -n | paste -sd, -)"
	agent_gateway_namespace="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].podSelector.matchLabels.\"app.kubernetes.io/name\" == \"agentops-agent\") | .from[0].namespaceSelector" "${rendered}")"
	collector_gateway_ports="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].podSelector.matchLabels.\"app.kubernetes.io/name\" == \"otel-collector\") | .ports[].port" "${rendered}" | sort -n | paste -sd, -)"
	collector_gateway_namespace="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].podSelector.matchLabels.\"app.kubernetes.io/name\" == \"otel-collector\") | .from[0].namespaceSelector" "${rendered}")"
	kagent_gateway_ports="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" == \"kagent\") | .ports[].port" "${rendered}" | sort -n | paste -sd, -)"
	kagent_gateway_instance="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" == \"kagent\") | .from[0].podSelector.matchLabels.\"app.kubernetes.io/instance\"" "${rendered}")"
	kagent_gateway_component="$(yq -r "select(${gateway_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" == \"kagent\") | .from[0].podSelector.matchLabels.\"app.kubernetes.io/component\"" "${rendered}")"
	gateway_protocols="$(yq -r "select(${gateway_ingress}) | .spec.ingress[].ports[].protocol" "${rendered}" | sort | paste -sd, -)"
	[[ "${gateway_ingress_selector}" == "agentgateway" ]]
	[[ "${gateway_ingress_rules}" == "3" ]]
	[[ "${gateway_ingress_source_counts}" == "1,1,1" ]]
	[[ "${agent_gateway_ports}" == "3000,4000" ]]
	[[ "${agent_gateway_namespace}" == "null" ]]
	[[ "${collector_gateway_ports}" == "15020" ]]
	[[ "${collector_gateway_namespace}" == "null" ]]
	[[ "${kagent_gateway_ports}" == "3000,4000" ]]
	[[ "${kagent_gateway_instance}" == "kagent" ]]
	[[ "${kagent_gateway_component}" == "controller" ]]
	[[ "${gateway_protocols}" == "TCP,TCP,TCP,TCP,TCP" ]]

	# Collector ingress is source-and-port specific in the completed reference:
	# gateway and kagent controller use OTLP/gRPC, the BYO agent uses OTLP/HTTP,
	# and only the local Prometheus overlay may scrape the metrics exporter.
	collector_ingress='.kind == "NetworkPolicy" and .metadata.name == "otel-collector-ingress"'
	collector_ingress_rules="$(yq -r "select(${collector_ingress}) | .spec.ingress | length" "${rendered}")"
	collector_ports="$(yq -r "select(${collector_ingress}) | .spec.ingress[].ports[].port" "${rendered}" | sort -n | paste -sd, -)"
	gateway_otel_port="$(yq -r "select(${collector_ingress}) | .spec.ingress[] | select(.from[0].podSelector.matchLabels.\"app.kubernetes.io/name\" == \"agentgateway\") | .ports[0].port" "${rendered}")"
	agent_otel_port="$(yq -r "select(${collector_ingress}) | .spec.ingress[] | select(.from[0].podSelector.matchLabels.\"app.kubernetes.io/name\" == \"agentops-agent\") | .ports[0].port" "${rendered}")"
	kagent_otel_namespace="$(yq -r "select(${collector_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector) | .from[0].namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\"" "${rendered}")"
	kagent_otel_instance="$(yq -r "select(${collector_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector) | .from[0].podSelector.matchLabels.\"app.kubernetes.io/instance\"" "${rendered}")"
	kagent_otel_component="$(yq -r "select(${collector_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector) | .from[0].podSelector.matchLabels.\"app.kubernetes.io/component\"" "${rendered}")"
	kagent_otel_port="$(yq -r "select(${collector_ingress}) | .spec.ingress[] | select(.from[0].namespaceSelector) | .ports[0].port" "${rendered}")"
	[[ "${collector_ingress_rules}" == "3" ]]
	[[ "${collector_ports}" == "4317,4317,4318" ]]
	[[ "${gateway_otel_port}" == "4317" ]]
	[[ "${agent_otel_port}" == "4318" ]]
	[[ "${kagent_otel_namespace}" == "kagent" ]]
	[[ "${kagent_otel_instance}" == "kagent" ]]
	[[ "${kagent_otel_component}" == "controller" ]]
	[[ "${kagent_otel_port}" == "4317" ]]

	metrics_ingress='.kind == "NetworkPolicy" and .metadata.name == "otel-collector-metrics-ingress"'
	metrics_ingress_count="$(yq -r "select(${metrics_ingress}) | .metadata.name" "${rendered}" | awk 'NF { count++ } END { print count + 0 }')"
	if [[ "${overlay}" == "local" ]]; then
		metrics_source="$(yq -r "select(${metrics_ingress}) | .spec.ingress[0].from[0].podSelector.matchLabels.\"app.kubernetes.io/name\"" "${rendered}")"
		metrics_port="$(yq -r "select(${metrics_ingress}) | .spec.ingress[0].ports[0].port" "${rendered}")"
		[[ "${metrics_ingress_count}" == "1" ]]
		[[ "${metrics_source}" == "prometheus" ]]
		[[ "${metrics_port}" == "8889" ]]
	else
		[[ "${metrics_ingress_count}" == "0" ]]
	fi

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
	backup_arguments="$(yq -r 'select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") | .spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .args[]' "${rendered}")"
	[[ "${backup_state_read_only}" == "true" ]]
	[[ "${backup_target_read_only}" == "false" ]]
	mlflow_memory_limit="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "mlflow") | .spec.template.spec.containers[0].resources.limits.memory' "${rendered}")"
	[[ "${mlflow_memory_limit}" == "2Gi" ]]
	if rg -Fx -- '--lock-file' <<<"${backup_arguments}" >/dev/null; then
		fail "backup CronJob must use the shared state-directory lock"
	fi

	if [[ "${overlay}" == "local" ]]; then
		[[ "${agent_model}" == "qwen3:4b-instruct" ]]
		[[ "${model_config}" == "qwen3:4b-instruct" ]]
	else
		[[ "${agent_model}" == "gemini-3.5-flash" ]]
		[[ "${model_config}" == "gemini-3.5-flash" ]]

		gateway_gsa="$(yq -r 'select(.kind == "ServiceAccount" and .metadata.name == "agentgateway") | .metadata.annotations."iam.gke.io/gcp-service-account"' "${rendered}")"
		mlflow_gsa="$(yq -r 'select(.kind == "ServiceAccount" and .metadata.name == "mlflow") | .metadata.annotations."iam.gke.io/gcp-service-account"' "${rendered}")"
		mlflow_bucket="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "mlflow") | .spec.template.spec.containers[] | select(.name == "mlflow") | .env[] | select(.name == "MLFLOW_ARTIFACTS_DESTINATION") | .value' "${rendered}")"
		gke_storage_classes="$(yq -r 'select(.kind == "PersistentVolumeClaim") | .spec.storageClassName' "${rendered}" | rg -v '^---$' | sort -u)"
		[[ "${gateway_gsa}" == "agentgateway@agentops-course-check.iam.gserviceaccount.com" ]]
		[[ "${mlflow_gsa}" == "mlflow@agentops-course-check.iam.gserviceaccount.com" ]]
		[[ "${mlflow_bucket}" == "gs://agentops-course-check-mlflow" ]]
		[[ "${gke_storage_classes}" == "agentops-standard" ]]

		for deployment in agentgateway agentops-mcp loki otel-collector; do
			deployment_cpu="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "'"${deployment}"'") | .spec.template.spec.containers[0].resources.requests.cpu' "${rendered}")"
			[[ "${deployment_cpu}" == "50m" ]]
		done
		mlflow_cpu="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "mlflow") | .spec.template.spec.containers[0].resources.requests.cpu' "${rendered}")"
		agent_cpu="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.resources.requests.cpu' "${rendered}")"
		[[ "${mlflow_cpu}" == "100m" ]]
		[[ "${agent_cpu}" == "100m" ]]

		vertex_backend_model="$(yq -r '.binds[] | select(.port == 4000) | .listeners[].routes[].backends[].ai.provider.vertex.model' infra/agentgateway/gke/config.yaml)"
		[[ "${vertex_backend_model}" == "google/gemini-3.5-flash" ]]

		dns_service_cidr="$(yq -r 'select(.kind == "NetworkPolicy" and .metadata.name == "dns-egress") | .spec.egress[].to[]? | select(.ipBlock) | .ipBlock.cidr' "${rendered}")"
		[[ "${dns_service_cidr}" == "10.30.0.10/32" ]]

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

# Resolve the NetworkPolicy selector against the pinned chart output. The
# instance label is shared by the controller, UI, and PostgreSQL workloads, so
# this check prevents a future selector change from admitting non-emitters.
kagent_chart_render="${tmp_dir}/kagent-chart.yaml"
helmfile --file infra/helmfile.yaml --quiet template >"${kagent_chart_render}"
kagent_otel_sources="$(
	yq -r '
		select(
			.kind == "Deployment" and
			.metadata.namespace == "kagent" and
			.spec.template.metadata.labels."app.kubernetes.io/instance" == "kagent" and
			.spec.template.metadata.labels."app.kubernetes.io/component" == "controller"
		) |
		.metadata.name
	' "${kagent_chart_render}" |
		sort |
		paste -sd, -
)"
[[ "${kagent_otel_sources}" == "kagent-controller" ]]

# Every committed custom-resource schema records the immutable CRD chart that
# produced it. A chart change therefore fails locally until schemas are reviewed
# and regenerated; a misspelled spec field proves validation is active.
kagent_crd_chart="$(yq -r '.releases[] | select(.name == "kagent-crds") | .chart' infra/helmfile.yaml)"
kagent_schema_sources="$(jq -r '."x-agentops-source-chart"' infra/kagent/schemas/*.json | sort -u)"
[[ "${kagent_schema_sources}" == "${kagent_crd_chart}" ]]
if kubeconform \
	-strict \
	-kubernetes-version 1.36.0 \
	-schema-location default \
	-schema-location "${kagent_schema_location}" \
	infra/kagent/fixtures/invalid-modelconfig-field.yaml >"${tmp_dir}/invalid-kagent.log" 2>&1; then
	echo "invalid kagent field passed the pinned offline schema" >&2
	exit 1
fi
grep -Fq "additional properties 'provder' not allowed" "${tmp_dir}/invalid-kagent.log"

# The broad ingress shown in Chapter 6 is an explicit, temporary fixture, not a
# resource included by either completed overlay. Keep its unsafe shape stable so
# the exercise remains reproducible and easy to delete.
kubeconform -strict -summary infra/k8s/exercises/otel-ingress-broad.yaml
exercise_policy_name="$(yq -r '.metadata.name' infra/k8s/exercises/otel-ingress-broad.yaml)"
exercise_sources="$(yq -r '.spec.ingress[0].from[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"' infra/k8s/exercises/otel-ingress-broad.yaml | sort | paste -sd, -)"
exercise_ports="$(yq -r '.spec.ingress[0].ports[].port' infra/k8s/exercises/otel-ingress-broad.yaml | sort -n | paste -sd, -)"
[[ "${exercise_policy_name}" == "exercise-broad-otel-ingress" ]]
[[ "${exercise_sources}" == "agentops,kagent" ]]
[[ "${exercise_ports}" == "4317,4318,8889" ]]
for rendered in "${tmp_dir}/local.yaml" "${tmp_dir}/gke.yaml"; do
	if yq -e 'select(.kind == "NetworkPolicy" and .metadata.name == "exercise-broad-otel-ingress")' "${rendered}" >/dev/null; then
		fail "${rendered}: temporary broad-ingress exercise leaked into a deployable overlay"
	fi
done

# The deployable CronJob must use the same versioned state CLI as the host
# wrappers. Keep this assertion exact so an illustrative one-off backup program
# cannot silently diverge from the tested snapshot contract.
backup_command="$(
	yq -r '
		select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") |
		.spec.jobTemplate.spec.template.spec.containers[] |
		select(.name == "backup") |
		.command | join(",")
	' "${tmp_dir}/local.yaml"
)"
backup_args="$(
	yq -r '
		select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") |
		.spec.jobTemplate.spec.template.spec.containers[] |
		select(.name == "backup") |
		.args | join(",")
	' "${tmp_dir}/local.yaml"
)"
[[ "${backup_command}" == "python,-m,agent.state,backup" ]]
[[ "${backup_args}" == "--state-dir,/app/state,--backup-root,/backups,--keep,7" ]]
./infra/scripts/backup-drill.sh

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

for gateway_config in infra/agentgateway/host/config.yaml infra/agentgateway/host/config-auth.yaml infra/agentgateway/k3d/config.yaml; do
	agentgateway --validate-only -f "${gateway_config}"
done
gke_gateway_config="${tmp_dir}/gke-gateway-config.yaml"
sed 's/__GCP_PROJECT_ID__/agentops-course-check/g' \
	infra/agentgateway/gke/config.yaml >"${gke_gateway_config}"
agentgateway --validate-only -f "${gke_gateway_config}"

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
grep -Fxq -- "cr.agentgateway.dev/agentgateway:v1.4.1@sha256:efd79355b89094a8225a9db465d9a01dc656b377f0bab458761b935a13231d29" "${host_container_args}"

# The wrapper must join a dedicated network, never the shared default bridge. On the default
# bridge the relay's MCP, A2A, and Ollama ports are reachable by every other container on the
# host — which silently contradicts the loopback-only guarantee Chapter 5.1 sells.
grep -Fxq -- "--network" "${host_container_args}"
container_network="$(awk '$0 == "--network" { getline; print; exit }' "${host_container_args}")"
[[ -n "${container_network}" && "${container_network}" != "bridge" && "${container_network}" != "host" ]]

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

# The three-profile gateway contract (Ch. 5.0). Host, k3d, and GKE may differ
# only in upstream address, model identity, caller authentication, and tracing;
# the ports, the MCP allowlist, the MCP failure mode, the token buckets, the
# prompt guards, and the browser origin are invariant across all three. Twenty-six
# hand-copied occurrences used to be verified by eye at the end of 5.0.
#
# The allowlist is compared against the tuple the agent's MCP client really
# pins, not against a literal list repeated here. Read it statically: importing
# the client initializes ADK's MCP integration and emits an experimental-feature
# warning during an otherwise pure infrastructure check. The Python test suite
# independently asserts that the server registers exactly this same set.
mcp_read_tools="$(
	agents/python/.venv/bin/python - <<'PY'
import ast
from pathlib import Path

tree = ast.parse(Path("agents/python/src/agent/mcp_client.py").read_text(encoding="utf-8"))
assignment = next(
	node
	for node in tree.body
	if isinstance(node, ast.Assign)
	and any(isinstance(target, ast.Name) and target.id == "MCP_READ_TOOL_NAMES" for target in node.targets)
)
print(",".join(sorted(ast.literal_eval(assignment.value))))
PY
)"
[[ -n "${mcp_read_tools}" ]]
mcp_read_tool_count="$(printf '%s\n' "${mcp_read_tools}" | tr ',' '\n' | wc -l)"
gateway_prompt_guard="$(
	yq -r '.binds[] | select(.port == 4000) | .listeners[].routes[].policies.ai.promptGuard' \
		infra/agentgateway/host/config.yaml
)"
[[ -n "${gateway_prompt_guard}" ]]

for gateway_config in infra/agentgateway/host/config.yaml infra/agentgateway/k3d/config.yaml infra/agentgateway/gke/config.yaml; do
	gateway_ports="$(yq -r '.binds[].port' "${gateway_config}" | sort -n | paste -sd, -)"
	[[ "${gateway_ports}" == "3000,3001,4000" ]]

	# Only rules of the exact `mcp.tool.name == "<tool>"` shape survive the sed,
	# so a broadened or misspelled rule drops out and fails the set comparison;
	# the count then catches an extra rule the sed dropped.
	mcp_rules='.binds[] | select(.port == 3000) | .listeners[].routes[].policies.mcpAuthorization.rules'
	gateway_tools="$(yq -r "${mcp_rules}[]" "${gateway_config}" |
		sed -n 's/^mcp\.tool\.name == "\([a-z_]*\)"$/\1/p' | sort | paste -sd, -)"
	gateway_rule_count="$(yq -r "${mcp_rules} | length" "${gateway_config}")"
	[[ "${gateway_tools}" == "${mcp_read_tools}" ]]
	[[ "${gateway_rule_count}" == "${mcp_read_tool_count}" ]]

	# An unreachable tool server must deny the request, never forward it.
	gateway_failure_mode="$(yq -r '.binds[] | select(.port == 3000) | .listeners[].routes[].backends[].mcp.failureMode' "${gateway_config}" | sort -u)"
	[[ "${gateway_failure_mode}" == "failClosed" ]]

	# Exactly one token bucket per listener, with the same numbers everywhere:
	# 120/60s MCP, 60/60s A2A, 30/60s model. A second bucket on a listener would
	# make these lists comma-joined and fail.
	for gateway_bucket in 3000:120 3001:60 4000:30; do
		rate_limit=".binds[] | select(.port == ${gateway_bucket%%:*}) | .listeners[].routes[].policies.localRateLimit"
		bucket_max_tokens="$(yq -r "${rate_limit}[].maxTokens" "${gateway_config}" | paste -sd, -)"
		bucket_tokens_per_fill="$(yq -r "${rate_limit}[].tokensPerFill" "${gateway_config}" | paste -sd, -)"
		bucket_fill_interval="$(yq -r "${rate_limit}[].fillInterval" "${gateway_config}" | paste -sd, -)"
		[[ "${bucket_max_tokens}" == "${gateway_bucket##*:}" ]]
		[[ "${bucket_tokens_per_fill}" == "${gateway_bucket##*:}" ]]
		[[ "${bucket_fill_interval}" == "60s" ]]
	done

	# Same request and response prompt guards on the model listener.
	profile_prompt_guard="$(yq -r '.binds[] | select(.port == 4000) | .listeners[].routes[].policies.ai.promptGuard' "${gateway_config}")"
	[[ "${profile_prompt_guard}" == "${gateway_prompt_guard}" ]]

	# The browser client is served from one fixed loopback origin. Keep every
	# port-forwardable A2A profile usable without opening CORS to arbitrary sites.
	cors='.binds[] | select(.port == 3001) | .listeners[].routes[].policies.cors'
	cors_origins=$(yq -r "${cors} | .allowOrigins | join(\",\")" "${gateway_config}")
	cors_methods=$(yq -r "${cors} | .allowMethods | join(\",\")" "${gateway_config}")
	cors_headers=$(yq -r "${cors} | .allowHeaders | join(\",\")" "${gateway_config}")
	[[ "${cors_origins}" == "http://localhost:8001" ]]
	[[ "${cors_methods}" == "GET,POST,OPTIONS" ]]
	[[ "${cors_headers}" == "content-type" ]]
done

# The Kubernetes profiles serve their own readiness endpoint, so the kubelet can
# probe something other than an open data port. The host profile gets the same
# address injected by the wrapper, asserted on the rendered config above.
for gateway_config in infra/agentgateway/k3d/config.yaml infra/agentgateway/gke/config.yaml; do
	profile_readiness_addr="$(yq -r '.config.readinessAddr' "${gateway_config}")"
	[[ "${profile_readiness_addr}" == "0.0.0.0:15021" ]]
done

compose_config="${tmp_dir}/observability-compose.json"
docker compose \
	--project-name agentops-observability \
	--file infra/observability/compose.yaml \
	config \
	--format json >"${compose_config}"
compose_services="$(jq -r '.services | keys[]' "${compose_config}" | sort | paste -sd, -)"
[[ "${compose_services}" == "alertmanager,grafana,loki,mlflow,otel-collector,prometheus" ]]
jq -e '
	.services |
	to_entries |
	all(
		.value.user != null and
		(.value.user | split(":")[0] != "0") and
		.value.read_only == true and
		(.value.cap_drop | index("ALL") != null) and
		(.value.security_opt | map(startswith("no-new-privileges")) | any) and
		(.value.mem_limit | tonumber) > 0 and
		.value.pids_limit > 0 and
		.value.cpus > 0
	)
' "${compose_config}" >/dev/null
(cd infra && skaffold diagnose --yaml-only -f skaffold.yaml -p local) >"${tmp_dir}/skaffold-local.yaml"
(cd infra && skaffold diagnose --yaml-only -f skaffold.yaml -p gke) >"${tmp_dir}/skaffold-gke.yaml"

rendered="${tmp_dir}/skaffold-render.yaml"
(
	cd infra || exit
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

# Same rule for the gateway: :3000 answers TCP the moment the listener binds,
# before backends, policies, and JWKS are usable, so both probes must target the
# pod-local readiness endpoint instead. It is deliberately not a Service port —
# the kubelet dials the pod IP.
gateway_container='select(.kind == "Deployment" and .metadata.name == "agentgateway") | .spec.template.spec.containers[] | select(.name == "agentgateway")'
gateway_readiness_port="$(yq -r "${gateway_container} | .ports[] | select(.name == \"readiness\") | .containerPort" "${rendered}")"
gateway_readiness_path="$(yq -r "${gateway_container} | .readinessProbe.httpGet.path" "${rendered}")"
gateway_readiness_target="$(yq -r "${gateway_container} | .readinessProbe.httpGet.port" "${rendered}")"
gateway_liveness_path="$(yq -r "${gateway_container} | .livenessProbe.httpGet.path" "${rendered}")"
gateway_liveness_target="$(yq -r "${gateway_container} | .livenessProbe.httpGet.port" "${rendered}")"
gateway_service_ports="$(yq -r 'select(.kind == "Service" and .metadata.name == "agentgateway") | .spec.ports[].port' "${rendered}" | sort -n | paste -sd, -)"
[[ "${gateway_readiness_port}" == "15021" ]]
[[ "${gateway_readiness_path}" == "/healthz/ready" ]]
[[ "${gateway_readiness_target}" == "readiness" ]]
[[ "${gateway_liveness_path}" == "/healthz/ready" ]]
[[ "${gateway_liveness_target}" == "readiness" ]]
[[ "${gateway_service_ports}" == "3000,3001,4000,15020" ]]

helmfile --file infra/helmfile.yaml --quiet lint --args '--quiet'

uv lock --directory infra/mlflow --check

tofu -chdir=infra/gcp fmt -check -recursive
tofu -chdir=infra/gcp init -backend=false -input=false -lockfile=readonly
tofu -chdir=infra/gcp validate
tflint --chdir=infra/gcp --minimum-failure-severity=warning
