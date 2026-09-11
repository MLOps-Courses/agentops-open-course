#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd go base
require_cmd yq gateway
require_cmd kubectl platform
require_cmd kubeconform platform
require_cmd kube-linter platform
require_cmd helmfile platform
require_cmd skaffold platform
require_cmd docker gateway
require_cmd tofu gcp
require_cmd tflint gcp
require_cmd promtool platform

# The Dockerfile must exercise the binary's shared build-info validator inside
# the builder. Shell-shape guards alone cannot enforce the typed release tuple.
grep -Fq '/out/agent version >/dev/null' agents/go/Dockerfile ||
	fail "agent Dockerfile does not validate its linked build identity"

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

# Arm cleanup before the first tool runs. Every command below can fail, and a
# trap installed later leaves `.agents/tmp/infra-check.*` behind on that path.
# INT and TERM exit explicitly so the EXIT trap still runs when a parallel task
# runner cancels this one.
cleanup() {
	rm -rf "${tmp_dir}"
	[[ ${cleanup_gateway_auth} == "0" ]] || rm -rf "${gateway_auth_dir}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

readonly kagent_schema_location='infra/kagent/schemas/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

# Rendering must never consult the maintainer's active cluster. Skaffold and
# Helmfile inspect KUBECONFIG even for offline renders, which can otherwise
# trigger cloud authentication or make the result depend on the current context.
export KUBECONFIG=/dev/null
source_identity_json="$(go -C tools run ./cmd/source-identity --root .. --mode development)"
export AGENT_BUILD_MODE=development
export AGENT_SOURCE_COMMIT
export AGENT_SOURCE_DIRTY
export AGENT_IMAGE_TAG
export AGENT_SOURCE_REVISION
export AGENT_SOURCE_TREE_DIGEST
export OCI_CREATED
export OCI_VERSION=development
AGENT_SOURCE_COMMIT="$(jq -er '.display' <<<"${source_identity_json}")"
AGENT_SOURCE_REVISION="$(jq -er '.revision // ""' <<<"${source_identity_json}")"
AGENT_SOURCE_TREE_DIGEST="$(jq -er '.tree_digest' <<<"${source_identity_json}")"
AGENT_SOURCE_DIRTY="$(json_flag '.dirty' "${source_identity_json}")"
AGENT_IMAGE_TAG="development-${AGENT_SOURCE_TREE_DIGEST#sha256:}"
OCI_CREATED="$(git show -s --format=%cI HEAD)"

skaffold_tag_template="$(yq -r '.build.tagPolicy.envTemplate.template' infra/skaffold.yaml)"
assert_eq "Skaffold source-identity tag" "${skaffold_tag_template}" '{{.AGENT_IMAGE_TAG}}'
skaffold_build_plan="$(cd infra && skaffold build --filename skaffold.yaml --profile local --dry-run --quiet)"
skaffold_rendered_tag="$(jq -er '.builds[0].tag' <<<"${skaffold_build_plan}")"
assert_eq "Skaffold rendered development tag" \
	"${skaffold_rendered_tag}" \
	"agentops-agent:${AGENT_IMAGE_TAG}"

# One source of truth for the alerting rules (Ch. 7.2): the Compose stack's file
# is a symlink to the overlay's, so both planes evaluate identical expressions.
# Assert the link itself — an editor or formatter that writes through it would
# silently restore two independently drifting copies.
[[ -L infra/observability/prometheus-rules.yml ]]
[[ ! -L infra/k8s/overlays/local/prometheus-rules.yaml ]]
prometheus_rules_link="$(readlink infra/observability/prometheus-rules.yml)"
assert_eq "Prometheus rules symlink" "${prometheus_rules_link}" "../k8s/overlays/local/prometheus-rules.yaml"
promtool check rules infra/observability/prometheus-rules.yml
# Every rule test in the directory, not a hand-maintained list: docs 7.2b sends
# learners here to add one, and a test nobody runs proves nothing.
for rule_test in infra/observability/tests/*.yml; do
	promtool test rules "${rule_test}"
done
collector_health_endpoint="$(yq -r '.extensions.health_check.endpoint' infra/k8s/base/otel-collector-config.yaml)"
collector_service_extensions="$(yq -r '.service.extensions | join(",")' infra/k8s/base/otel-collector-config.yaml)"
assert_eq "collector health extension endpoint" "${collector_health_endpoint}" "0.0.0.0:13133"
assert_eq "collector enabled extensions" "${collector_service_extensions}" "health_check"

# Traces leave the collector over OTLP/HTTP to Tempo, in both planes. A wrong
# exporter id is not a startup error the learner sees — the collector accepts
# `otlp` (gRPC) against Tempo's HTTP port and then drops every batch — so assert
# the exact exporter, its endpoint, and that the traces pipeline names it.
for collector_config in infra/observability/otel-collector.yaml infra/k8s/base/otel-collector-config.yaml; do
	trace_exporters="$(yq -r '.service.pipelines.traces.exporters | sort | join(",")' "${collector_config}")"
	tempo_endpoint="$(yq -r '.exporters."otlp_http/tempo".endpoint' "${collector_config}")"
	loki_endpoint="$(yq -r '.exporters."otlp_http/loki".endpoint' "${collector_config}")"
	assert_eq "${collector_config} trace exporters" "${trace_exporters}" "otlp_http/tempo,span_metrics"
	assert_eq "${collector_config} Tempo endpoint" "${tempo_endpoint}" "http://tempo:4318"
	assert_eq "${collector_config} Loki endpoint" "${loki_endpoint}" "http://loki:3100/otlp"
done

# Both Tempo configs must accept OTLP on the port the collector writes to and
# serve their query API on the port Grafana and the readiness check use.
for tempo_config in infra/observability/tempo.yaml infra/k8s/base/tempo-config.yaml; do
	tempo_otlp_endpoint="$(yq -r '.distributor.receivers.otlp.protocols.http.endpoint' "${tempo_config}")"
	tempo_http_port="$(yq -r '.server.http_listen_port' "${tempo_config}")"
	tempo_usage_report="$(yq -r '.usage_report.reporting_enabled' "${tempo_config}")"
	assert_eq "${tempo_config} OTLP receiver" "${tempo_otlp_endpoint}" "0.0.0.0:4318"
	assert_eq "${tempo_config} query port" "${tempo_http_port}" "3200"
	# The account-free promise: no component may phone an upstream vendor.
	assert_eq "${tempo_config} usage reporting" "${tempo_usage_report}" "false"
done

# Correlation is only a lesson if it resolves in both directions: a span must
# reach its logs and a log line must reach its trace. One direction is half a
# feature, and neither link fails loudly when its target uid is wrong.
grafana_datasources=infra/observability/grafana/datasources.yaml
tempo_datasource_url="$(yq -r '.datasources[] | select(.uid == "tempo") | .url' "${grafana_datasources}")"
traces_to_logs_uid="$(yq -r '.datasources[] | select(.uid == "tempo") | .jsonData.tracesToLogsV2.datasourceUid' "${grafana_datasources}")"
derived_field_uid="$(yq -r '.datasources[] | select(.uid == "loki") | .jsonData.derivedFields[0].datasourceUid' "${grafana_datasources}")"
derived_field_matcher="$(yq -r '.datasources[] | select(.uid == "loki") | .jsonData.derivedFields[0].matcherType + ":" + .jsonData.derivedFields[0].matcherRegex' "${grafana_datasources}")"
derived_field_url="$(yq -r '.datasources[] | select(.uid == "loki") | .jsonData.derivedFields[0].url' "${grafana_datasources}")"
assert_eq "Grafana Tempo datasource URL" "${tempo_datasource_url}" "http://tempo:3200"
assert_eq "Grafana trace-to-logs target" "${traces_to_logs_uid}" "loki"
assert_eq "Grafana log-to-trace target" "${derived_field_uid}" "tempo"
assert_eq "Grafana log-to-trace matcher" "${derived_field_matcher}" "label:trace_id"
# Grafana interpolates `$VAR` while loading a provisioning file, so a single
# `$` here would provision an empty link that still looks configured.
assert_eq "Grafana log-to-trace query" "${derived_field_url}" "\$\${__value.raw}"
if rg -n '/env/[0-9]+/value' infra/k8s/overlays/*/kustomization.yaml; then
	fail "overlay environment patches must select entries by name"
fi

# `scale` is validated alongside the two deployable overlays because 6.9 states
# its rendering, schema validation, and lint as proved. It layers on `../local`,
# so every local expectation below holds for it except the two figures it
# deliberately changes: the MCP replica count and the pod quota.
for overlay in local gke scale; do
	rendered="${tmp_dir}/${overlay}.yaml"
	if [[ ${overlay} == gke ]]; then
		GCP_PROJECT_ID=agentops-course-check \
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

	default_deny_ingress='select(.kind == "NetworkPolicy" and .metadata.name == "default-deny-ingress")'
	default_deny_ingress_selector="$(yq -o=json -I=0 "${default_deny_ingress} | .spec.podSelector" "${rendered}")"
	default_deny_ingress_types="$(yq -r "${default_deny_ingress} | .spec.policyTypes | join(\",\")" "${rendered}")"
	assert_eq "${overlay} default-deny ingress selector" "${default_deny_ingress_selector}" "{}"
	assert_eq "${overlay} default-deny ingress types" "${default_deny_ingress_types}" "Ingress"

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
	assert_eq "${overlay} A2A selector" "${a2a_selector}" "agentops-agent"
	assert_eq "${overlay} A2A ingress rule count" "${a2a_ingress_rules}" "1"
	assert_eq "${overlay} A2A source count" "${a2a_sources}" "1"
	assert_eq "${overlay} A2A source" "${a2a_source_name}" "agentgateway"
	assert_eq "${overlay} A2A source namespace" "${a2a_source_namespace}" "null"
	assert_eq "${overlay} A2A port" "${a2a_ingress_port}" "8080"

	agent_egress_selector="$(yq -r 'select(.kind == "NetworkPolicy" and .metadata.name == "agent-egress") | .spec.podSelector.matchLabels."app.kubernetes.io/name"' "${rendered}")"
	gateway_agent_target="$(yq -r 'select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-egress") | .spec.egress[] | select(.ports[].port == 8080) | .to[0].podSelector.matchLabels."app.kubernetes.io/name"' "${rendered}")"
	assert_eq "${overlay} agent egress selector" "${agent_egress_selector}" "agentops-agent"
	assert_eq "${overlay} gateway agent target" "${gateway_agent_target}" "agentops-agent"

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
	assert_eq "${overlay} gateway ingress selector" "${gateway_ingress_selector}" "agentgateway"
	assert_eq "${overlay} gateway ingress rule count" "${gateway_ingress_rules}" "3"
	assert_eq "${overlay} gateway source counts" "${gateway_ingress_source_counts}" "1,1,1"
	assert_eq "${overlay} agent gateway ports" "${agent_gateway_ports}" "3000,4000"
	assert_eq "${overlay} agent gateway namespace" "${agent_gateway_namespace}" "null"
	assert_eq "${overlay} collector gateway ports" "${collector_gateway_ports}" "15020"
	assert_eq "${overlay} collector gateway namespace" "${collector_gateway_namespace}" "null"
	assert_eq "${overlay} kagent gateway ports" "${kagent_gateway_ports}" "3000,4000"
	assert_eq "${overlay} kagent gateway instance" "${kagent_gateway_instance}" "kagent"
	assert_eq "${overlay} kagent gateway component" "${kagent_gateway_component}" "controller"
	assert_eq "${overlay} gateway protocols" "${gateway_protocols}" "TCP,TCP,TCP,TCP,TCP"

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
	assert_eq "${overlay} collector ingress rule count" "${collector_ingress_rules}" "3"
	assert_eq "${overlay} collector ingress ports" "${collector_ports}" "4317,4317,4318"
	assert_eq "${overlay} gateway OTLP port" "${gateway_otel_port}" "4317"
	assert_eq "${overlay} agent OTLP port" "${agent_otel_port}" "4318"
	assert_eq "${overlay} kagent OTLP namespace" "${kagent_otel_namespace}" "kagent"
	assert_eq "${overlay} kagent OTLP instance" "${kagent_otel_instance}" "kagent"
	assert_eq "${overlay} kagent OTLP component" "${kagent_otel_component}" "controller"
	assert_eq "${overlay} kagent OTLP port" "${kagent_otel_port}" "4317"

	collector_health_port="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "otel-collector") | .spec.template.spec.containers[] | select(.name == "collector") | .ports[] | select(.name == "health") | .containerPort' "${rendered}")"
	collector_readiness="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "otel-collector") | .spec.template.spec.containers[] | select(.name == "collector") | .readinessProbe.httpGet.path + ":" + .readinessProbe.httpGet.port' "${rendered}")"
	collector_liveness="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "otel-collector") | .spec.template.spec.containers[] | select(.name == "collector") | .livenessProbe.httpGet.path + ":" + .livenessProbe.httpGet.port' "${rendered}")"
	assert_eq "${overlay} collector health port" "${collector_health_port}" "13133"
	assert_eq "${overlay} collector readiness endpoint" "${collector_readiness}" "/:health"
	assert_eq "${overlay} collector liveness endpoint" "${collector_liveness}" "/:health"

	metrics_ingress='.kind == "NetworkPolicy" and .metadata.name == "otel-collector-metrics-ingress"'
	metrics_ingress_count="$(yq -r "select(${metrics_ingress}) | .metadata.name" "${rendered}" | awk 'NF { count++ } END { print count + 0 }')"
	if [[ "${overlay}" != "gke" ]]; then
		metrics_source="$(yq -r "select(${metrics_ingress}) | .spec.ingress[0].from[0].podSelector.matchLabels.\"app.kubernetes.io/name\"" "${rendered}")"
		metrics_port="$(yq -r "select(${metrics_ingress}) | .spec.ingress[0].ports[0].port" "${rendered}")"
		assert_eq "${overlay} collector metrics policy count" "${metrics_ingress_count}" "1"
		assert_eq "${overlay} collector metrics source" "${metrics_source}" "prometheus"
		assert_eq "${overlay} collector metrics port" "${metrics_port}" "8889"
	else
		assert_eq "GKE collector metrics policy count" "${metrics_ingress_count}" "0"
	fi

	agent_model="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_MODEL") | .value' "${rendered}")"
	agent_provider="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_MODEL_PROVIDER") | .value' "${rendered}")"
	agent_bind_host="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_A2A_BIND_HOST") | .value' "${rendered}")"
	agent_a2a_max_llm_calls="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_A2A_MAX_LLM_CALLS") | .value' "${rendered}")"
	agent_writes_disabled="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_WRITES_DISABLED") | .value' "${rendered}")"
	adk_capture_message_content="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS") | .value' "${rendered}")"
	otel_traces_sampler="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "OTEL_TRACES_SAMPLER") | .value' "${rendered}")"
	retired_gateway_flag="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env | map(select(.name == "AGENT_GATEWAY_ENABLED")) | length' "${rendered}")"
	model_config="$(yq -r 'select(.kind == "ModelConfig" and .metadata.name == "agentgateway") | .spec.model' "${rendered}")"
	pod_quota="$(yq -r 'select(.kind == "ResourceQuota" and .metadata.name == "agentops-compute") | .spec.hard.pods' "${rendered}")"
	# The scale overlay raises the budget by the autoscaler's three extra MCP
	# pods plus the 6.9 PostgreSQL fixture; that arithmetic is spelled out in
	# infra/k8s/overlays/scale/kustomization.yaml and pinned here.
	expected_pod_quota=13
	[[ ${overlay} != scale ]] || expected_pod_quota=17
	assert_eq "${overlay} pod quota" "${pod_quota}" "${expected_pod_quota}"
	assert_eq "${overlay} agent model provider" "${agent_provider}" "openai-compatible"
	assert_eq "${overlay} A2A bind host" "${agent_bind_host}" "0.0.0.0"
	assert_eq "${overlay} A2A model-call cap" "${agent_a2a_max_llm_calls}" "4"
	assert_eq "${overlay} agent writes disabled" "${agent_writes_disabled}" "true"
	assert_eq "${overlay} ADK message content capture" "${adk_capture_message_content}" "false"
	assert_eq "${overlay} OTel trace sampler" "${otel_traces_sampler}" "always_off"
	assert_eq "${overlay} retired gateway flag count" "${retired_gateway_flag}" "0"

	backup_state_read_only="$(yq -r 'select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") | .spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .volumeMounts[] | select(.name == "state") | (.readOnly // false)' "${rendered}")"
	backup_target_read_only="$(yq -r 'select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") | .spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .volumeMounts[] | select(.name == "backups") | (.readOnly // false)' "${rendered}")"
	backup_arguments="$(yq -r 'select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") | .spec.jobTemplate.spec.template.spec.containers[] | select(.name == "backup") | .args[]' "${rendered}")"
	assert_eq "${overlay} backup state read-only" "${backup_state_read_only}" "true"
	assert_eq "${overlay} backup target writable" "${backup_target_read_only}" "false"
	tempo_memory_limit="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "tempo") | .spec.template.spec.containers[0].resources.limits.memory' "${rendered}")"
	assert_eq "${overlay} Tempo memory limit" "${tempo_memory_limit}" "1Gi"
	if rg -Fx -- '--lock-file' <<<"${backup_arguments}" >/dev/null; then
		fail "backup CronJob must use the shared state-directory lock"
	fi

	if [[ "${overlay}" != "gke" ]]; then
		assert_eq "${overlay} agent model" "${agent_model}" "qwen3:4b-instruct"
		assert_eq "${overlay} ModelConfig model" "${model_config}" "qwen3:4b-instruct"
		agent_pii_base_url="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_PII_MODEL_BASE_URL") | .value' "${rendered}")"
		pii_egress_rule='select(.kind == "NetworkPolicy" and .metadata.name == "agent-egress") | .spec.egress[] | select(.ports[]?.port == 11434)'
		pii_egress_count="$(yq -r "${pii_egress_rule} | .ports[0].port" "${rendered}" | awk 'NF { count++ } END { print count + 0 }')"
		pii_egress_cidr="$(yq -r "${pii_egress_rule} | .to[0].ipBlock.cidr" "${rendered}")"
		assert_eq "${overlay} PII model URL" "${agent_pii_base_url}" "http://host.k3d.internal:11434/v1"
		assert_eq "${overlay} PII model egress rule count" "${pii_egress_count}" "1"
		assert_eq "${overlay} PII model egress CIDR" "${pii_egress_cidr}" "0.0.0.0/0"
	else
		assert_eq "GKE agent model" "${agent_model}" "gemini-3.5-flash"
		assert_eq "GKE ModelConfig model" "${model_config}" "gemini-3.5-flash"
		agent_pii_enabled="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.env[] | select(.name == "AGENT_PII_MODEL_ENABLED") | .value' "${rendered}")"
		assert_eq "GKE PII model disabled" "${agent_pii_enabled}" "false"

		gateway_gsa="$(yq -r 'select(.kind == "ServiceAccount" and .metadata.name == "agentgateway") | .metadata.annotations."iam.gke.io/gcp-service-account"' "${rendered}")"
		# agentgateway is the only workload with a Google identity: it is the sole
		# component that calls a cloud API (Vertex). Every other workload, tracing
		# included, persists to a PersistentVolumeClaim, so a second annotated
		# ServiceAccount would be unexplained cloud privilege.
		annotated_service_accounts="$(yq -r 'select(.kind == "ServiceAccount" and .metadata.annotations."iam.gke.io/gcp-service-account" != null) | .metadata.name' "${rendered}" | sort | paste -sd, -)"
		gke_storage_classes="$(yq -r 'select(.kind == "PersistentVolumeClaim") | .spec.storageClassName' "${rendered}" | rg -v '^---$' | sort -u)"
		assert_eq "GKE gateway service account" "${gateway_gsa}" "agentgateway@agentops-course-check.iam.gserviceaccount.com"
		assert_eq "GKE annotated service accounts" "${annotated_service_accounts}" "agentgateway"
		assert_eq "GKE storage classes" "${gke_storage_classes}" "agentops-standard"

		for deployment in agentgateway agentops-mcp loki otel-collector tempo; do
			deployment_cpu="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "'"${deployment}"'") | .spec.template.spec.containers[0].resources.requests.cpu' "${rendered}")"
			assert_eq "GKE ${deployment} CPU request" "${deployment_cpu}" "50m"
		done
		agent_cpu="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.resources.requests.cpu' "${rendered}")"
		assert_eq "GKE agent CPU request" "${agent_cpu}" "100m"

		vertex_backend_model="$(yq -r '.routes[] | select(.name == "llm") | .backends[].ai.provider.vertex.model' infra/agentgateway/gke/config.yaml)"
		assert_eq "GKE Vertex backend model" "${vertex_backend_model}" "google/gemini-3.5-flash"

		dns_service_cidr="$(yq -r 'select(.kind == "NetworkPolicy" and .metadata.name == "dns-egress") | .spec.egress[].to[]? | select(.ipBlock) | .ipBlock.cidr' "${rendered}")"
		assert_eq "GKE DNS service CIDR" "${dns_service_cidr}" "10.30.0.10/32"

		# Terraform selects Calico, not Dataplane V2. Lock the workload to the
		# corresponding GKE metadata endpoint and reject the incompatible one.
		if grep -Fq "169.254.169.254" "${rendered}"; then
			echo "GKE overlay contains the Dataplane V2 metadata endpoint, but the cluster uses Calico" >&2
			exit 1
		fi
		# agentgateway is the sole Workload Identity consumer, so the occurrence
		# count below is also an assertion that no second workload quietly
		# reacquires the metadata route.
		wif_cidr="169.254.169.252/32"
		wif_policy="agentgateway-egress"
		wif_rule='select(.kind == "NetworkPolicy" and .metadata.name == "'"${wif_policy}"'") | .spec.egress[] | select(.to[0].ipBlock.cidr == "'"${wif_cidr}"'")'
		wif_rule_count="$(yq -r "${wif_rule} | .to[0].ipBlock.cidr" "${rendered}" | awk 'NF { count++ } END { print count + 0 }')"
		wif_to_counts="$(yq -r "${wif_rule} | .to | length" "${rendered}" | sort -n | paste -sd, -)"
		wif_ports="$(yq -r "${wif_rule} | .ports[].port" "${rendered}" | sort -n | paste -sd, -)"
		wif_protocols="$(yq -r "${wif_rule} | .ports[].protocol" "${rendered}" | sort | paste -sd, -)"
		wif_cidr_count="$(grep -Fc "${wif_cidr}" "${rendered}")"
		assert_eq "${wif_policy} WIF rule count" "${wif_rule_count}" "1"
		assert_eq "${wif_policy} WIF destination count" "${wif_to_counts}" "1"
		assert_eq "${wif_policy} WIF ports" "${wif_ports}" "987,988"
		assert_eq "${wif_policy} WIF protocols" "${wif_protocols}" "TCP,TCP"
		assert_eq "GKE WIF CIDR occurrence count" "${wif_cidr_count}" "1"
	fi

	# kustomize silently ignores a patch whose target matches nothing, so a
	# renamed base resource would leave this overlay unscaled with every check
	# above still green. Pin the figures 6.9 quotes against the actual render.
	if [[ "${overlay}" == "scale" ]]; then
		mcp_replicas="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp") | .spec.replicas' "${rendered}")"
		mcp_autoscaler='select(.kind == "HorizontalPodAutoscaler" and .metadata.name == "agentops-mcp")'
		mcp_autoscaler_target="$(yq -r "${mcp_autoscaler} | .spec.scaleTargetRef.kind + \"/\" + .spec.scaleTargetRef.name" "${rendered}")"
		mcp_autoscaler_floor="$(yq -r "${mcp_autoscaler} | .spec.minReplicas" "${rendered}")"
		mcp_autoscaler_ceiling="$(yq -r "${mcp_autoscaler} | .spec.maxReplicas" "${rendered}")"
		# The absence of a replica count is the assertion: the autoscaler owns the
		# floor alone, because a pinned `spec.replicas` and an HPA on the same
		# Deployment contend on every apply.
		assert_eq "scale MCP replica count" "${mcp_replicas}" "null"
		assert_eq "scale MCP autoscaler target" "${mcp_autoscaler_target}" "Deployment/agentops-mcp"
		assert_eq "scale MCP autoscaler floor" "${mcp_autoscaler_floor}" "2"
		assert_eq "scale MCP autoscaler ceiling" "${mcp_autoscaler_ceiling}" "4"
	fi
done

# Resolve the NetworkPolicy selector against the pinned chart output. The
# instance label is shared by the controller, UI, and PostgreSQL workloads, so
# this check prevents a future selector change from admitting non-emitters.
kagent_chart_render="${tmp_dir}/kagent-chart.yaml"
helmfile --file infra/helmfile.yaml --quiet template >"${kagent_chart_render}"
kagent_workload_images="$({
	yq -r '
		select(
			.kind == "Deployment" or
			.kind == "StatefulSet" or
			.kind == "DaemonSet" or
			.kind == "Job"
		) |
		.spec.template.spec |
		(.initContainers[]?.image, .containers[]?.image)
	' "${kagent_chart_render}"
	yq -r '
		select(.kind == "CronJob") |
		.spec.jobTemplate.spec.template.spec |
		(.initContainers[]?.image, .containers[]?.image)
	' "${kagent_chart_render}"
} | rg -v '^---$' | sort -u)"
[[ -n "${kagent_workload_images}" ]]
while IFS= read -r image; do
	# A digest-pinned chart is insufficient when its rendered workloads still
	# select mutable tags. Validate the actual PodSpecs that Kubernetes receives.
	if [[ ! "${image}" =~ @sha256:[0-9a-f]{64}$ ]]; then
		fail "kagent chart renders a mutable workload image: ${image}"
	fi
done <<<"${kagent_workload_images}"
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
# The shared-session-store fixture is quarantined the same way and gets the same
# treatment: nobody applies it from an overlay, so nothing else would notice the
# day its apiVersion stops matching the cluster this course pins.
kubeconform -strict -kubernetes-version 1.36.0 -summary infra/k8s/exercises/sessions-postgres.yaml
exercise_policy_name="$(yq -r '.metadata.name' infra/k8s/exercises/otel-ingress-broad.yaml)"
exercise_sources="$(yq -r '.spec.ingress[0].from[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"' infra/k8s/exercises/otel-ingress-broad.yaml | sort | paste -sd, -)"
exercise_ports="$(yq -r '.spec.ingress[0].ports[].port' infra/k8s/exercises/otel-ingress-broad.yaml | sort -n | paste -sd, -)"
[[ "${exercise_policy_name}" == "exercise-broad-otel-ingress" ]]
[[ "${exercise_sources}" == "agentops,kagent" ]]
[[ "${exercise_ports}" == "4317,4318,8889" ]]
# Select the name rather than asserting with `yq -e`: a clean overlay is the
# expected outcome here, and `-e` reports that absence as a scary `Error: no
# matches found` line in an otherwise passing gate. A malformed rendered file
# still exits non-zero and fails the script under `set -e`.
for rendered in "${tmp_dir}/local.yaml" "${tmp_dir}/gke.yaml" "${tmp_dir}/scale.yaml"; do
	leaked_exercise_policy="$(yq -r '
	  select(.kind == "NetworkPolicy" and .metadata.name == "exercise-broad-otel-ingress")
	  | .metadata.name
	' "${rendered}")"
	[[ -z ${leaked_exercise_policy} ]] ||
		fail "${rendered}: temporary broad-ingress exercise leaked into a deployable overlay"
done

# The declarative specialist is an opt-in comparison, not a second required
# runtime. Validate both its exact kagent references and the permissions that
# disappear with `kubectl delete -f` while keeping it out of every overlay.
kagent_interop_exercise=infra/kagent/exercises/incident-reader.yaml
kubeconform \
	-strict \
	-kubernetes-version 1.36.0 \
	-schema-location default \
	-schema-location "${kagent_schema_location}" \
	-summary \
	"${kagent_interop_exercise}"
kube-linter lint --fail-if-no-objects-found --with-color=false "${kagent_interop_exercise}"
yq -e '
  select(.kind == "Agent" and .metadata.name == "incident-reader") |
  .spec.type == "Declarative" and
  .spec.declarative.runtime == "go" and
  .spec.declarative.modelConfig == "agentgateway" and
  (.spec.declarative.tools | length) == 1 and
  .spec.declarative.tools[0].type == "McpServer" and
  .spec.declarative.tools[0].mcpServer.name == "agentops-tools" and
  .spec.declarative.tools[0].mcpServer.kind == "RemoteMCPServer" and
  .spec.declarative.tools[0].mcpServer.apiGroup == "kagent.dev" and
  (.spec.declarative.tools[0].mcpServer.toolNames | join(",")) ==
    "list_incidents,get_incident,get_service_status,search_service_logs,get_runbook,search_runbooks" and
  .spec.declarative.tools[0].mcpServer.requireApproval == null and
  (.spec.declarative.deployment.env | length) == 1 and
  .spec.declarative.deployment.env[0].name == "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT" and
  .spec.declarative.deployment.env[0].value == "false" and
  .spec.declarative.deployment.podSecurityContext.runAsNonRoot == true and
  .spec.declarative.deployment.podSecurityContext.seccompProfile.type == "RuntimeDefault" and
  .spec.declarative.deployment.securityContext.allowPrivilegeEscalation == false and
  (.spec.declarative.deployment.securityContext.capabilities.drop | contains(["ALL"])) and
  .spec.declarative.memory == null and
  .spec.skills == null and
  .spec.sandbox == null
' "${kagent_interop_exercise}" >/dev/null
# The namespace enforces restricted Pod Security and kagent copies these blocks
# into the generated pod with no default of its own, so both Agent shapes must
# declare them or their pods are rejected at admission. This yq assertion is the
# only gate that can hold it: kubeconform checks the custom resource against the
# CRD schema, and kube-linter finds no PodSpec inside a custom resource to lint.
yq -e '
  select(.kind == "Agent" and .metadata.name == "agentops-agent") |
  .spec.byo.deployment.podSecurityContext.runAsNonRoot == true and
  .spec.byo.deployment.podSecurityContext.seccompProfile.type == "RuntimeDefault" and
  .spec.byo.deployment.securityContext.allowPrivilegeEscalation == false and
  (.spec.byo.deployment.securityContext.capabilities.drop | contains(["ALL"]))
' infra/kagent/agent.yaml >/dev/null
interop_resource_count="$(yq -r -N 'select(.metadata.labels."agentops.course/exercise" == "kagent-interop") | .kind' "${kagent_interop_exercise}" | wc -l)"
interop_policy_count="$(yq -r -N 'select(.kind == "NetworkPolicy" and .metadata.labels."agentops.course/exercise" == "kagent-interop") | .metadata.name' "${kagent_interop_exercise}" | wc -l)"
assert_eq "kagent interop removable resource count" "${interop_resource_count}" "4"
assert_eq "kagent interop NetworkPolicy count" "${interop_policy_count}" "3"
yq -e '
  select(.kind == "NetworkPolicy" and .metadata.name == "incident-reader-ingress") |
  .spec.podSelector.matchLabels."app.kubernetes.io/name" == "incident-reader" and
  .spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kagent" and
  .spec.ingress[0].from[0].podSelector.matchLabels."app.kubernetes.io/component" == "controller" and
  .spec.ingress[0].ports[0].port == 8080
' "${kagent_interop_exercise}" >/dev/null
yq -e '
  select(.kind == "NetworkPolicy" and .metadata.name == "incident-reader-egress") |
  .spec.podSelector.matchLabels."app.kubernetes.io/name" == "incident-reader" and
  ([.spec.egress[].ports[].port] | sort | join(",")) == "3000,4000,8083"
' "${kagent_interop_exercise}" >/dev/null
yq -e '
  select(.kind == "NetworkPolicy" and .metadata.name == "incident-reader-egress") |
  .spec.egress[] | select(.ports[].port == 8083) |
  [
    .to[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name",
    .to[0].podSelector.matchLabels."app.kubernetes.io/instance",
    .to[0].podSelector.matchLabels."app.kubernetes.io/component"
  ] | join(",") | . == "kagent,kagent,controller"
' "${kagent_interop_exercise}" >/dev/null
for rendered in "${tmp_dir}/local.yaml" "${tmp_dir}/gke.yaml" "${tmp_dir}/scale.yaml"; do
	if yq -e 'select(
      (.kind == "Agent" and .metadata.name == "incident-reader") or
      .metadata.labels."agentops.course/exercise" == "kagent-interop"
    )' "${rendered}" >/dev/null 2>&1; then
		fail "${rendered}: optional kagent interoperability exercise leaked into a deployable overlay"
	fi
done

# The deployable CronJob must use the same versioned state CLI as the host
# wrappers. Keep this assertion exact so an illustrative one-off backup program
# cannot silently diverge from the tested snapshot contract. The container sets
# `args` and never `command`: the image's entrypoint is the agent binary, and an
# overridden entrypoint is exactly how a second, untested CLI would creep in.
backup_command="$(
	yq -r '
		select(.kind == "CronJob" and .metadata.name == "agentops-state-backup") |
		.spec.jobTemplate.spec.template.spec.containers[] |
		select(.name == "backup") |
		.command // "unset"
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
[[ "${backup_command}" == "unset" ]]
[[ "${backup_args}" == "state,backup,--state-dir,/app/state,--backup-root,/backups,--keep,7" ]]
./infra/scripts/backup-drill.sh
./infra/scripts/test-assert-platform-build-info.sh

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

# Standalone v1.4 names each gateway and attaches top-level routes explicitly.
# Keep this structural check separate from schema validation: `binds` remains
# accepted for compatibility, so a valid file can still teach the deprecated API.
for gateway_config in \
	infra/agentgateway/host/config.yaml \
	infra/agentgateway/host/config-auth.yaml \
	infra/agentgateway/k3d/config.yaml \
	infra/agentgateway/gke/config.yaml; do
	yq -e '
      .binds == null and
      (.gateways | keys | sort | join(",")) == "a2a,llm,mcp" and
      ([.gateways[].port] | sort | join(",")) == "3000,3001,4000" and
      ([.routes[].name] | sort | join(",")) == "a2a,llm,mcp" and
      ([.routes[] | (((.gateways | length) == 1) and (.name == .gateways[0]))] | all_c(.))
    ' "${gateway_config}" >/dev/null

	mcp_policy='.routes[] | select(.name == "mcp") | .policies'
	yq -e "
      (${mcp_policy}.timeout.requestTimeout == \"5s\") and
      (${mcp_policy}.timeout.backendRequestTimeout == \"2s\") and
      (${mcp_policy}.retry.attempts == 2) and
      (${mcp_policy}.retry.backoff == \"100ms\") and
      (${mcp_policy}.retry.codes | join(\",\")) == \"502,503,504\" and
      (${mcp_policy}.delay == null) and
      ([.routes[] | select(.name != \"mcp\") | .policies.retry == null] | all_c(.))
    " "${gateway_config}" >/dev/null
done

# A self-hosted Ollama token has no provider fee. The local catalog therefore
# records exact zero-dollar rates for attribution while GKE stays unpriced until
# an owner supplies a dated provider catalog; neither value claims a bill.
for gateway_config in \
	infra/agentgateway/host/config.yaml \
	infra/agentgateway/host/config-auth.yaml \
	infra/agentgateway/k3d/config.yaml; do
	yq -e '
      (.config.modelCatalog | length) == 1 and
      (.config.modelCatalog[0].inline.providers | keys | join(",")) == "openai" and
      (.config.modelCatalog[0].inline.providers.openai.models | keys | join(",")) == "qwen3:4b-instruct" and
      .config.modelCatalog[0].inline.providers.openai.models."qwen3:4b-instruct".rates.input == "0" and
      .config.modelCatalog[0].inline.providers.openai.models."qwen3:4b-instruct".rates.output == "0"
    ' "${gateway_config}" >/dev/null
done
yq -e '.config.modelCatalog == null' infra/agentgateway/gke/config.yaml >/dev/null

# Two client shapes, one governed model route. The agent speaks Responses and the
# evaluation judge (evals/judge.go) speaks chat-completions, and .env.example tells
# the reader to point the judge at the gateway on :4000; both entries must therefore
# stay declared or that instruction becomes false again. A `pathOverride` would pin
# every route type to one upstream endpoint, which is precisely how chat-completions
# calls were rewritten onto /v1/responses and answered with an empty body. The
# upstream path belongs to the resolved route type, so no profile may pin it.
# scripts/smoke-host.sh proves the runtime behavior; this pins the shipped shape.
for gateway_config in \
	infra/agentgateway/host/config.yaml \
	infra/agentgateway/host/config-auth.yaml \
	infra/agentgateway/k3d/config.yaml; do
	model_route_types="$(yq -r '
      .routes[] | select(.name == "llm") | .policies.ai.routes |
      to_entries | sort_by(.key) | map(.key + "=" + .value) | join(",")
    ' "${gateway_config}")"
	assert_eq "${gateway_config} model route types" \
		"${model_route_types}" \
		"/v1/chat/completions=completions,/v1/responses=responses"
	yq -e '[.routes[] | select(.name == "llm") | .backends[].ai.pathOverride == null] | all_c(.)' \
		"${gateway_config}" >/dev/null
done
# GKE is deliberately excluded: its backend is Vertex, not a co-hosted Ollama, and no
# evaluation path points a judge at it. Admitting a second client shape there would
# add a request translation nobody on this course can exercise, so that profile keeps
# the single Responses route the agent actually uses.
gke_model_route_types="$(yq -r '
  .routes[] | select(.name == "llm") | .policies.ai.routes |
  to_entries | map(.key + "=" + .value) | join(",")
' infra/agentgateway/gke/config.yaml)"
assert_eq "GKE model route types" "${gke_model_route_types}" "/v1/responses=responses"

# The host file stays the canonical process-oriented profile. The Docker
# wrapper derives a network-correct copy without committing a second config.
host_container_config="${tmp_dir}/host-container.yaml"
infra/scripts/gateway-host.sh render >"${host_container_config}"
agentgateway --validate-only -f "${host_container_config}"
container_mcp="$(yq -r '.routes[] | select(.name == "mcp") | .backends[].mcp.targets[].mcp.host' "${host_container_config}")"
container_a2a="$(yq -r '.routes[] | select(.name == "a2a") | .backends[].host' "${host_container_config}")"
container_model="$(yq -r '.routes[] | select(.name == "llm") | .backends[].ai.hostOverride' "${host_container_config}")"
container_pii_webhooks="$(yq -r '
  .routes[] | select(.name == "llm") | .policies.ai.promptGuard |
  [.request[], .response[]] | map(select(.webhook != null).webhook.target.host) | join(",")
' "${host_container_config}")"
container_stats_addr="$(yq -r '.config.statsAddr' "${host_container_config}")"
container_readiness_addr="$(yq -r '.config.readinessAddr' "${host_container_config}")"
container_admin_addr="$(yq -r '.config.adminAddr' "${host_container_config}")"
assert_eq "host container MCP upstream" "${container_mcp}" "http://host.docker.internal:8000/mcp"
assert_eq "host container A2A upstream" "${container_a2a}" "host.docker.internal:8080"
assert_eq "host container model upstream" "${container_model}" "host.docker.internal:11434"
assert_eq "host container PII webhooks" "${container_pii_webhooks}" "host.docker.internal:8080,host.docker.internal:8080"
assert_eq "host container stats address" "${container_stats_addr}" "0.0.0.0:15020"
assert_eq "host container readiness address" "${container_readiness_addr}" "0.0.0.0:15021"
assert_eq "host container admin address" "${container_admin_addr}" "off"

# The resilience lab is a deterministic render of the same read-only MCP route:
# its six-second delay exceeds the existing five-second total timeout. Keeping
# this opt-in proves the exact policy without slowing or weakening normal turns.
host_resilience_config="${tmp_dir}/host-resilience.yaml"
AGENTOPS_GATEWAY_RESILIENCE_LAB=timeout \
	infra/scripts/gateway-host.sh render >"${host_resilience_config}"
agentgateway --validate-only -f "${host_resilience_config}"
host_resilience_delay="$(yq -r '.routes[] | select(.name == "mcp") | .policies.delay.duration' "${host_resilience_config}")"
host_resilience_timeout="$(yq -r '.routes[] | select(.name == "mcp") | .policies.timeout.requestTimeout' "${host_resilience_config}")"
host_resilience_attempts="$(yq -r '.routes[] | select(.name == "mcp") | .policies.retry.attempts' "${host_resilience_config}")"
assert_eq "host resilience delay" \
	"${host_resilience_delay}" \
	"6s"
assert_eq "host resilience total timeout" \
	"${host_resilience_timeout}" \
	"5s"
assert_eq "host resilience attempts" \
	"${host_resilience_attempts}" \
	"2"

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
auth_certs="$(yq -r '.gateways[] | select(.tls != null) | .tls.cert' "${host_auth_container_config}" | sort -u)"
auth_keys="$(yq -r '.gateways[] | select(.tls != null) | .tls.key' "${host_auth_container_config}" | sort -u)"
auth_jwks="$(yq -r '.routes[] | select(.policies.jwtAuth.jwks.file != null) | .policies.jwtAuth.jwks.file' "${host_auth_container_config}" | sort -u)"
auth_mcp="$(yq -r '.routes[] | select(.name == "mcp") | .backends[].mcp.targets[].mcp.host' "${host_auth_container_config}")"
auth_a2a="$(yq -r '.routes[] | select(.name == "a2a") | .backends[].host' "${host_auth_container_config}")"
auth_model="$(yq -r '.routes[] | select(.name == "llm") | .backends[].ai.hostOverride' "${host_auth_container_config}")"
auth_pii_webhooks="$(yq -r '
  .routes[] | select(.name == "llm") | .policies.ai.promptGuard |
  [.request[], .response[]] | map(select(.webhook != null).webhook.target.host) | join(",")
' "${host_auth_container_config}")"
assert_eq "host auth certificate mount" "${auth_certs}" "/etc/agentgateway/auth/tls-cert.pem"
assert_eq "host auth key mount" "${auth_keys}" "/etc/agentgateway/auth/tls-key.pem"
assert_eq "host auth JWKS mount" "${auth_jwks}" "/etc/agentgateway/auth/jwks.json"
assert_eq "host auth MCP upstream" "${auth_mcp}" "http://host.docker.internal:8000/mcp"
assert_eq "host auth A2A upstream" "${auth_a2a}" "host.docker.internal:8080"
assert_eq "host auth model upstream" "${auth_model}" "host.docker.internal:11434"
assert_eq "host auth PII webhooks" "${auth_pii_webhooks}" "host.docker.internal:8080,host.docker.internal:8080"

# Inspect the actual argument array produced by the wrapper, rather than a
# parallel policy description that could drift from `docker run`.
host_container_args="${tmp_dir}/host-container.args"
infra/scripts/gateway-host.sh args >"${host_container_args}"
grep -Fxq -- "--user" "${host_container_args}"
container_user="$(awk '$0 == "--user" { getline; print; exit }' "${host_container_args}")"
container_cap_drop="$(awk '$0 == "--cap-drop" { getline; print; exit }' "${host_container_args}")"
container_security_opt="$(awk '$0 == "--security-opt" { getline; print; exit }' "${host_container_args}")"
container_tmpfs="$(awk '$0 == "--tmpfs" { getline; print; exit }' "${host_container_args}")"
assert_eq "host container user" "${container_user}" "65532:65532"
grep -Fxq -- "--read-only" "${host_container_args}"
assert_eq "host container dropped capabilities" "${container_cap_drop}" "ALL"
assert_eq "host container security option" "${container_security_opt}" "no-new-privileges=true"
assert_eq "host container tmpfs" "${container_tmpfs}" "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777"
grep -Fxq -- "cr.agentgateway.dev/agentgateway:v1.4.1@sha256:efd79355b89094a8225a9db465d9a01dc656b377f0bab458761b935a13231d29" "${host_container_args}"

# The wrapper must join a dedicated network, never the shared default bridge. On the default
# bridge the relay's MCP, A2A, and Ollama ports are reachable by every other container on the
# host — which silently contradicts the loopback-only guarantee Chapter 5.1 sells.
grep -Fxq -- "--network" "${host_container_args}"
container_network="$(awk '$0 == "--network" { getline; print; exit }' "${host_container_args}")"
[[ -n "${container_network}" && "${container_network}" != "bridge" && "${container_network}" != "host" ]]
container_network_alias="$(awk '$0 == "--network-alias" { getline; print; exit }' "${host_container_args}")"
assert_eq "host gateway network alias" "${container_network_alias}" "agentops-gateway"

awk '$0 == "--publish" { getline; print }' "${host_container_args}" >"${tmp_dir}/host-container.published"
published_count="$(awk 'NF { count++ } END { print count + 0 }' "${tmp_dir}/host-container.published")"
assert_eq "host published port count" "${published_count}" "5"
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
# browser origin are invariant across all three. The two OpenAI profiles also
# share mask+webhook guards; Vertex stays reject-only because it has no local Ollama. Twenty-six
# hand-copied occurrences used to be verified by eye at the end of 5.0.
#
# The allowlist is compared against the list the agent's MCP client really pins, not
# against a literal repeated here. It is asked of the compiled package rather than parsed
# out of the source, because the names are typed constants declared in three other files
# and a text scrape would have to re-resolve them. The Go test suite independently asserts
# that the server registers exactly this same set.
mcp_read_tools_program="$(pwd)/${tmp_dir}/mcp-read-tools.go"
cat >"${mcp_read_tools_program}" <<'GO'
package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
)

func main() {
	names := compose.MCPReadToolNames()
	sort.Strings(names)
	fmt.Println(strings.Join(names, ","))
}
GO
mcp_read_tools="$(cd agents/go && go run "${mcp_read_tools_program}")"
[[ -n "${mcp_read_tools}" ]]
mcp_read_tool_count="$(printf '%s\n' "${mcp_read_tools}" | tr ',' '\n' | wc -l)"
prompt_guard_shape='
  .routes[] | select(.name == "llm") | .policies.ai.promptGuard |
  ((.request[], .response[]) | select(.webhook != null) | .webhook.target.host) = "WEBHOOK_TARGET"
'
gateway_prompt_guard="$(yq -r "${prompt_guard_shape}" infra/agentgateway/host/config.yaml)"
[[ -n "${gateway_prompt_guard}" ]]

for gateway_config in infra/agentgateway/host/config.yaml infra/agentgateway/k3d/config.yaml infra/agentgateway/gke/config.yaml; do
	gateway_ports="$(yq -r '.gateways[].port' "${gateway_config}" | sort -n | paste -sd, -)"
	[[ "${gateway_ports}" == "3000,3001,4000" ]]

	# Only rules of the exact `mcp.tool.name == "<tool>"` shape survive the sed,
	# so a broadened or misspelled rule drops out and fails the set comparison;
	# the count then catches an extra rule the sed dropped.
	mcp_rules='.routes[] | select(.name == "mcp") | .policies.mcpAuthorization.rules'
	gateway_tools="$(yq -r "${mcp_rules}[]" "${gateway_config}" |
		sed -n 's/^mcp\.tool\.name == "\([a-z_]*\)"$/\1/p' | sort | paste -sd, -)"
	gateway_rule_count="$(yq -r "${mcp_rules} | length" "${gateway_config}")"
	[[ "${gateway_tools}" == "${mcp_read_tools}" ]]
	[[ "${gateway_rule_count}" == "${mcp_read_tool_count}" ]]

	# An unreachable tool server must deny the request, never forward it.
	gateway_failure_mode="$(yq -r '.routes[] | select(.name == "mcp") | .backends[].mcp.failureMode' "${gateway_config}" | sort -u)"
	[[ "${gateway_failure_mode}" == "failClosed" ]]

	# Fixed bucket registry per route, identical in every profile: one request
	# bucket of 120/60s on MCP and 60/60s on A2A, and two buckets on the model
	# route — 30 requests/60s plus 100000 LLM tokens/60s, the spend ceiling
	# docs 7.3b teaches. Each field is comma-joined across the route's buckets,
	# so an added, removed, reordered, or retyped bucket fails here instead of
	# silently contradicting the prose. `null` is the absent `type` key, which
	# agentgateway reads as the default `requests`.
	for gateway_bucket in \
		"mcp|120|60s|null" \
		"a2a|60|60s|null" \
		"llm|30,100000|60s,60s|null,tokens"; do
		IFS='|' read -r bucket_route bucket_sizes bucket_intervals bucket_types <<<"${gateway_bucket}"
		rate_limit=".routes[] | select(.name == \"${bucket_route}\") | .policies.localRateLimit"
		bucket_max_tokens="$(yq -r "${rate_limit}[].maxTokens" "${gateway_config}" | paste -sd, -)"
		bucket_tokens_per_fill="$(yq -r "${rate_limit}[].tokensPerFill" "${gateway_config}" | paste -sd, -)"
		bucket_fill_interval="$(yq -r "${rate_limit}[].fillInterval" "${gateway_config}" | paste -sd, -)"
		bucket_type="$(yq -r "${rate_limit}[].type" "${gateway_config}" | paste -sd, -)"
		[[ "${bucket_max_tokens}" == "${bucket_sizes}" ]]
		[[ "${bucket_tokens_per_fill}" == "${bucket_sizes}" ]]
		[[ "${bucket_fill_interval}" == "${bucket_intervals}" ]]
		[[ "${bucket_type}" == "${bucket_types}" ]]
	done

	# The OpenAI-compatible profiles share one policy shape while their webhook
	# targets remain network-specific. Normalize only that address before the
	# structural comparison so a policy change still fails this contract.
	profile_prompt_guard="$(yq -r "${prompt_guard_shape}" "${gateway_config}")"
	if [[ "${gateway_config}" != infra/agentgateway/gke/config.yaml ]]; then
		[[ "${profile_prompt_guard}" == "${gateway_prompt_guard}" ]]
	else
		gke_guard_shape="$(yq -r '
          [.request[], .response[]] |
          {"count": length, "actions": map(.regex.action), "webhooks": map(select(.webhook != null)) | length}
        ' <<<"${profile_prompt_guard}")"
		gke_guard_count="$(yq -r '.count' <<<"${gke_guard_shape}")"
		gke_guard_actions="$(yq -r '.actions | join(",")' <<<"${gke_guard_shape}")"
		gke_guard_webhooks="$(yq -r '.webhooks' <<<"${gke_guard_shape}")"
		[[ "${gke_guard_count}" == "2" ]]
		[[ "${gke_guard_actions}" == "reject,reject" ]]
		[[ "${gke_guard_webhooks}" == "0" ]]
	fi

	# The browser client is served from one fixed loopback origin. Keep every
	# port-forwardable A2A profile usable without opening CORS to arbitrary sites.
	cors='.routes[] | select(.name == "a2a") | .policies.cors'
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
assert_eq "observability Compose services" "${compose_services}" "alertmanager,grafana,loki,otel-collector,prometheus,tempo"
compose_network="$(jq -r '.networks.default.name' "${compose_config}")"
prometheus_gateway_target="$(yq -r '.scrape_configs[] | select(.job_name == "agentgateway") | .static_configs[0].targets[0]' infra/observability/prometheus.yml)"
assert_eq "observability shared gateway network" "${compose_network}" "agentops-host-gateway-net"
assert_eq "Prometheus gateway target" "${prometheus_gateway_target}" "agentops-gateway:15020"
prometheus_tempo_target="$(yq -r '.scrape_configs[] | select(.job_name == "tempo") | .static_configs[0].targets[0]' infra/observability/prometheus.yml)"
assert_eq "Prometheus Tempo target" "${prometheus_tempo_target}" "tempo:3200"
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
		--images agentops-agent=agentops-agent:infra-check
) >"${rendered}"
agent_image="$(yq -r 'select(.kind == "Agent" and .metadata.name == "agentops-agent") | .spec.byo.deployment.image' "${rendered}")"
mcp_image="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "agentops-mcp") | .spec.template.spec.containers[] | select(.name == "mcp") | .image' "${rendered}")"
# Tempo ships as an upstream release, so Skaffold must leave its reference
# alone. A tag-only image here would let the cluster and the Compose stack run
# two different trace stores while both call themselves the same version.
tempo_image="$(yq -r 'select(.kind == "Deployment" and .metadata.name == "tempo") | .spec.template.spec.containers[] | select(.name == "tempo") | .image' "${rendered}")"
compose_tempo_image="$(jq -r '.services.tempo.image' "${compose_config}")"
[[ "${agent_image}" == "${mcp_image}" ]]
[[ "${agent_image##*/}" == "agentops-agent:infra-check" ]]
assert_eq "rendered Tempo image" "${tempo_image}" "${compose_tempo_image}"
[[ "${tempo_image}" == *"@sha256:"* ]]

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

tofu -chdir=infra/gcp fmt -check -recursive
# TF_DATA_DIR must be absolute: `-chdir` moves OpenTofu into infra/gcp first, so
# a repository-relative path would create (and leave behind) a second scratch
# tree under infra/gcp instead of the one this script removes on exit.
tofu_data_dir="${PWD}/${tmp_dir}/tofu-data"
TF_DATA_DIR="${tofu_data_dir}" tofu -chdir=infra/gcp \
	init -backend=false -input=false -lockfile=readonly
TF_DATA_DIR="${tofu_data_dir}" tofu -chdir=infra/gcp validate
TF_DATA_DIR="${tofu_data_dir}" tofu -chdir=infra/gcp test
tflint --chdir=infra/gcp --minimum-failure-severity=warning
