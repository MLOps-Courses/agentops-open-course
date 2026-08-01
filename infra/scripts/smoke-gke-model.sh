#!/usr/bin/env bash

# Prove the optional GKE data plane against its exact source, context, live
# configuration, Vertex function-response turn, and one read-only A2A tool use.

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/lib.sh"

require_cmd curl gcp
require_cmd git base
require_cmd jq base
require_cmd kubectl gcp
require_cmd tofu gcp
require_cmd yq gcp

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${repo_dir}" || exit

head_commit="$(git rev-parse --verify 'HEAD^{commit}')"
[[ ${head_commit} =~ ^[0-9a-f]{40}$ ]] || fail "HEAD did not resolve to a full commit SHA"
source_tag="$(git rev-parse --short=7 "${head_commit}")"
working_tree_status="$(git status --porcelain)"
[[ -z ${working_tree_status} ]] || fail "GKE smoke requires a clean working tree"

source_gateway_model="$(
	yq -er \
		'.binds[] | select(.port == 4000) | .listeners[].routes[].backends[].ai.provider.vertex.model' \
		infra/agentgateway/gke/config.yaml
)"
[[ ${source_gateway_model} == google/* ]] ||
	fail "the source Vertex model must use the google/ publisher prefix"
model="${source_gateway_model#google/}"
source_agent_model="$(yq -er '.spec.byo.deployment.env[] | select(.name == "AGENT_MODEL") | .value' infra/kagent/agent.yaml)"
source_model_config="$(yq -er '.spec.model' infra/kagent/modelconfig.yaml)"
source_gateway_image="$(
	yq -er '
      select(.kind == "Deployment" and .metadata.name == "agentgateway")
      | .spec.template.spec.containers[]
      | select(.name == "agentgateway")
      | .image
    ' infra/k8s/base/agentgateway.yaml
)"
[[ -n ${source_gateway_image} && ${source_gateway_image} != *$'\n'* ]] ||
	fail "expected one source agentgateway image"
[[ ${source_agent_model} == "${model}" && ${source_model_config} == "${model}" ]] ||
	fail "the source gateway, Agent, and ModelConfig model owners disagree"

project_id="$(tofu -chdir=infra/gcp output -raw project_id)"
cluster_name="$(tofu -chdir=infra/gcp output -raw cluster_name)"
cluster_zone="$(tofu -chdir=infra/gcp output -raw cluster_zone)"
repository="$(tofu -chdir=infra/gcp output -raw artifact_registry_repository)"
expected_agent_image_prefix="${repository}/agentops-agent:${source_tag}@sha256:"
expected_context="gke_${project_id}_${cluster_zone}_${cluster_name}"
current_context="$(kubectl config current-context)"
[[ ${current_context} == "${expected_context}" ]] ||
	fail "kubectl context is ${current_context}; expected ${expected_context}"

kube() {
	kubectl --context "${expected_context}" --namespace agentops "$@"
}

agent_workload_model() {
	jq -er '
      [
        (
          .spec.template.spec.containers
          | if length == 1 then .[0] else error("expected one Agent workload container") end
        )
        | .env[]
        | select(.name == "AGENT_MODEL")
        | .value
      ]
      | if length == 1 then .[0] else error("expected one workload AGENT_MODEL") end
    '
}

agent_workload_image() {
	jq -er '
      .spec.template.spec.containers
      | if length == 1 then .[0].image else error("expected one Agent workload image") end
    '
}

assert_agent_image() {
	local label="$1"
	local image="$2"
	local digest

	[[ ${image} == "${expected_agent_image_prefix}"* ]] ||
		fail "${label} is outside the exact HEAD-tagged Artifact Registry repository"
	digest="${image#"${expected_agent_image_prefix}"}"
	[[ ${digest} =~ ^[0-9a-f]{64}$ ]] ||
		fail "${label} does not end in one full sha256 digest"
}

live_agent="$(kube get agent.kagent.dev/agentops-agent --output json)"
live_agent_model="$(
	jq -er '
      [.spec.byo.deployment.env[] | select(.name == "AGENT_MODEL") | .value]
      | if length == 1 then .[0] else error("expected one AGENT_MODEL") end
    ' <<<"${live_agent}"
)"
live_agent_image="$(jq -er '.spec.byo.deployment.image' <<<"${live_agent}")"
[[ ${live_agent_model} == "${model}" ]] ||
	fail "live Agent model ${live_agent_model} does not match source ${model}"
assert_agent_image "live Agent declaration" "${live_agent_image}"

live_agent_deployment=""
live_workload_model=""
live_workload_image=""
reconciled=false
for _ in {1..300}; do
	if live_agent_deployment="$(kube get deployment/agentops-agent --output json 2>/dev/null)" &&
		live_workload_model="$(agent_workload_model <<<"${live_agent_deployment}" 2>/dev/null)" &&
		live_workload_image="$(agent_workload_image <<<"${live_agent_deployment}" 2>/dev/null)" &&
		[[ ${live_workload_model} == "${live_agent_model}" && ${live_workload_image} == "${live_agent_image}" ]]; then
		reconciled=true
		break
	fi
	sleep 1
done
[[ ${reconciled} == true ]] ||
	fail "kagent did not reconcile the Agent image and model within 300 seconds"

for deployment in agentgateway agentops-agent; do
	kube rollout status "deployment/${deployment}" --timeout 300s
done

live_agent_deployment="$(kube get deployment/agentops-agent --output json)"
live_workload_model="$(agent_workload_model <<<"${live_agent_deployment}")"
live_workload_image="$(agent_workload_image <<<"${live_agent_deployment}")"
jq -e '.status.observedGeneration >= .metadata.generation' <<<"${live_agent_deployment}" >/dev/null ||
	fail "the Agent workload has not observed its current generation"
[[ ${live_workload_model} == "${model}" ]] ||
	fail "live Agent workload model ${live_workload_model} does not match source ${model}"
[[ ${live_workload_image} == "${live_agent_image}" ]] ||
	fail "live Agent declaration and generated workload images disagree"
assert_agent_image "live Agent workload" "${live_workload_image}"

live_gateway_deployment="$(kube get deployment/agentgateway --output json)"
jq -e '.status.observedGeneration >= .metadata.generation' <<<"${live_gateway_deployment}" >/dev/null ||
	fail "the agentgateway Deployment has not observed its current generation"
live_gateway_image="$(
	jq -er '
      [.spec.template.spec.containers[] | select(.name == "agentgateway") | .image]
      | if length == 1 then .[0] else error("expected one live agentgateway image") end
    ' <<<"${live_gateway_deployment}"
)"
[[ ${live_gateway_image} == "${source_gateway_image}" ]] ||
	fail "live agentgateway image ${live_gateway_image} does not match source ${source_gateway_image}"
if ! gateway_config_name="$(
	jq -er '
      [
        .spec.template.spec.volumes[]
        | select(.name == "config")
        | .configMap.name
        | select(startswith("agentgateway-config-"))
      ]
      | if length == 1 then .[0] else error("expected one mounted agentgateway ConfigMap") end
    ' <<<"${live_gateway_deployment}"
)"; then
	fail "the live agentgateway Deployment does not mount one generated config"
fi
gateway_config_map="$(kube get "configmap/${gateway_config_name}" --output json)"
live_gateway_config="$(jq -er '.data["config.yaml"]' <<<"${gateway_config_map}")"
live_gateway_model="$(
	yq -er \
		'.binds[] | select(.port == 4000) | .listeners[].routes[].backends[].ai.provider.vertex.model' \
		<<<"${live_gateway_config}"
)"
live_model_config="$(kube get modelconfig.kagent.dev/agentgateway --output json)"
live_model_config_model="$(jq -er '.spec.model' <<<"${live_model_config}")"

[[ ${live_gateway_model} == "${source_gateway_model}" ]] ||
	fail "live gateway model ${live_gateway_model} does not match source ${source_gateway_model}"
[[ ${live_model_config_model} == "${model}" ]] ||
	fail "live ModelConfig model ${live_model_config_model} does not match source ${model}"

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/agentops-gke-smoke.XXXXXX")"
port_forward_pid=""
cleanup() {
	if [[ -n ${port_forward_pid} ]] && kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
		kill -TERM "${port_forward_pid}" >/dev/null 2>&1 || true
		sleep 0.1
		if kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
			kill -KILL "${port_forward_pid}" >/dev/null 2>&1 || true
		fi
		wait "${port_forward_pid}" 2>/dev/null || true
	fi
	rm -rf -- "${work_dir}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

port_forward_log="${work_dir}/port-forward.log"
kubectl \
	--context "${expected_context}" \
	--namespace agentops \
	port-forward \
	--address 127.0.0.1 \
	service/agentgateway \
	:3001 :4000 :15020 >"${port_forward_log}" 2>&1 &
port_forward_pid=$!

a2a_port=""
model_port=""
metrics_port=""
for _ in {1..100}; do
	if ! kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
		[[ ! -f ${port_forward_log} ]] || cat "${port_forward_log}" >&2
		fail "the exact-context agentgateway port-forward stopped before becoming ready"
	fi
	if [[ -f ${port_forward_log} ]]; then
		a2a_port="$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) -> 3001$/\1/p' "${port_forward_log}" | head -n 1)"
		model_port="$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) -> 4000$/\1/p' "${port_forward_log}" | head -n 1)"
		metrics_port="$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) -> 15020$/\1/p' "${port_forward_log}" | head -n 1)"
	fi
	[[ -z ${a2a_port} || -z ${model_port} || -z ${metrics_port} ]] || break
	sleep 0.1
done
[[ ${a2a_port} =~ ^[0-9]+$ && ${model_port} =~ ^[0-9]+$ && ${metrics_port} =~ ^[0-9]+$ ]] || {
	[[ ! -f ${port_forward_log} ]] || cat "${port_forward_log}" >&2
	fail "the exact-context agentgateway port-forward did not publish all three random loopback ports"
}

show_response() {
	local label="$1"
	local response_file="$2"

	printf '%s response (first 4096 bytes):\n' "${label}" >&2
	head -c 4096 "${response_file}" >&2 || true
	printf '\n' >&2
}

scrape_metrics() {
	local response_file="$1"

	if ! curl --fail-with-body --silent --show-error --max-time 10 \
		--output "${response_file}" \
		"http://127.0.0.1:${metrics_port}/metrics"; then
		show_response "agentgateway metrics" "${response_file}"
		fail "the exact-context agentgateway metrics scrape failed"
	fi
}

get_incident_call_count() {
	local metrics_file="$1"

	awk '
      /^agentgateway_mcp_requests_total\{/ \
        && /method="tools\/call"/ \
        && /resource_type="tool"/ \
        && /resource="get_incident"/ {
          total += $NF
        }
      END { printf "%.17g\n", total + 0 }
    ' "${metrics_file}"
}

model_endpoint="http://127.0.0.1:${model_port}/v1/chat/completions"
first_payload="$(
	jq -cn \
		--arg model "${model}" \
		'{
          model: $model,
          messages: [{
            role: "user",
            content: "Call course_probe exactly once with label gke, then report the supplied status."
          }],
          tools: [{
            type: "function",
            function: {
              name: "course_probe",
              description: "Return the supplied GKE acceptance status.",
              parameters: {
                type: "object",
                properties: {label: {type: "string"}},
                required: ["label"]
              }
            }
          }],
          tool_choice: {
            type: "function",
            function: {name: "course_probe"}
          },
          temperature: 0,
          max_tokens: 512,
          stream: false
        }'
)"
first_response="${work_dir}/model-first.json"
if ! curl --fail-with-body --silent --show-error --max-time 180 \
	--header 'Authorization: Bearer agentgateway' \
	--header 'Content-Type: application/json' \
	--data "${first_payload}" \
	--output "${first_response}" \
	"${model_endpoint}"; then
	show_response "first model turn" "${first_response}"
	fail "the model did not return the forced function call"
fi

if ! assistant_message="$(
	jq -ce '
      .choices[0].message
      | select(.role == "assistant")
      | select((.tool_calls | length) == 1)
      | select(.tool_calls[0].type == "function")
      | select(.tool_calls[0].function.name == "course_probe")
      | select(.tool_calls[0].function.arguments | fromjson | .label == "gke")
      | select(.tool_calls[0].id | type == "string" and length > 0)
    ' "${first_response}"
)"; then
	show_response "first model turn" "${first_response}"
	fail "the first model turn did not contain the exact course_probe call"
fi
tool_call_id="$(jq -er '.tool_calls[0].id' <<<"${assistant_message}")"

second_payload="$(
	jq -cn \
		--arg model "${model}" \
		--arg tool_call_id "${tool_call_id}" \
		--argjson assistant "${assistant_message}" \
		'{
          model: $model,
          messages: [
            {
              role: "user",
              content: "Call course_probe exactly once with label gke, then report the supplied status."
            },
            $assistant,
            {
              role: "tool",
              tool_call_id: $tool_call_id,
              content: "{\"status\":\"ready\"}"
            }
          ],
          tools: [{
            type: "function",
            function: {
              name: "course_probe",
              description: "Return the supplied GKE acceptance status.",
              parameters: {
                type: "object",
                properties: {label: {type: "string"}},
                required: ["label"]
              }
            }
          }],
          temperature: 0,
          max_tokens: 512,
          stream: false
        }'
)"
second_response="${work_dir}/model-second.json"
if ! curl --fail-with-body --silent --show-error --max-time 180 \
	--header 'Authorization: Bearer agentgateway' \
	--header 'Content-Type: application/json' \
	--data "${second_payload}" \
	--output "${second_response}" \
	"${model_endpoint}"; then
	show_response "model function-response turn" "${second_response}"
	fail "the gateway/provider pair rejected the function-response turn"
fi
if ! jq -e '
  .choices[0].finish_reason == "stop"
  and (
    .choices[0].message.content
    | type == "string"
      and (gsub("\\s"; "") | length > 0)
      and (ascii_downcase | contains("ready"))
  )
' "${second_response}" >/dev/null; then
	show_response "model function-response turn" "${second_response}"
	fail "the function-response turn did not finish with the supplied ready status"
fi

smoke_id="gke-model-smoke-${head_commit:0:12}-$$-${RANDOM}"
a2a_payload="$(
	jq -cn \
		--arg request_id "${smoke_id}" \
		--arg message_id "${smoke_id}-message" \
		'{
          jsonrpc: "2.0",
          id: $request_id,
          method: "message/send",
          params: {
            message: {
              kind: "message",
              role: "user",
              messageId: $message_id,
              parts: [{
                kind: "text",
                text: "Use get_incident exactly once to retrieve INC-002. Include its exact incident ID, severity code, title, and current status. Do not perform any write action."
              }]
            }
          }
        }'
)"
a2a_response="${work_dir}/a2a.json"
metrics_before_file="${work_dir}/metrics-before.txt"
scrape_metrics "${metrics_before_file}"
get_incident_calls_before="$(get_incident_call_count "${metrics_before_file}")"
if ! curl --fail-with-body --silent --show-error --max-time 300 \
	--header 'Content-Type: application/json' \
	--data "${a2a_payload}" \
	--output "${a2a_response}" \
	"http://127.0.0.1:${a2a_port}/"; then
	show_response "A2A investigation" "${a2a_response}"
	fail "the A2A investigation request failed"
fi

if ! a2a_text="$(
	jq -er '
      select(.error == null)
      | .result
      | select(.kind == "task")
      | select(.status.state == "completed")
      | select((.metadata.adk_error_code // "") == "")
      | select((.status.message.metadata.adk_error_code // "") == "")
      | [
          .artifacts[]?.parts[]?,
          .status.message.parts[]?
        ]
      | map(select(.kind == "text") | .text)
      | join("\n")
      | select(gsub("\\s"; "") | length > 0)
    ' "${a2a_response}"
)"; then
	show_response "A2A investigation" "${a2a_response}"
	fail "the A2A investigation did not reach a text-bearing completed task"
fi
for expected_evidence in INC-002 SEV1 "Inventory service unavailable"; do
	if [[ ${a2a_text} != *"${expected_evidence}"* ]]; then
		show_response "A2A investigation" "${a2a_response}"
		fail "the A2A investigation omitted seed evidence: ${expected_evidence}"
	fi
done

get_incident_calls_after="${get_incident_calls_before}"
for _ in {1..20}; do
	metrics_after_file="${work_dir}/metrics-after.txt"
	scrape_metrics "${metrics_after_file}"
	get_incident_calls_after="$(get_incident_call_count "${metrics_after_file}")"
	if awk \
		-v before="${get_incident_calls_before}" \
		-v after="${get_incident_calls_after}" \
		'BEGIN { exit !(after == before + 1) }'; then
		break
	fi
	sleep 0.25
done
if ! awk \
	-v before="${get_incident_calls_before}" \
	-v after="${get_incident_calls_after}" \
	'BEGIN { exit !(after == before + 1) }'; then
	fail "get_incident counter delta was not exactly one: before=${get_incident_calls_before}, after=${get_incident_calls_after}"
fi

printf 'GKE model tool loop and read-only A2A retrieval passed for %s at %s\n' \
	"${model}" "${head_commit}"
