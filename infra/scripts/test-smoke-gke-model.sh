#!/usr/bin/env bash

scripts_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${scripts_dir}/../.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${repo_dir}/scripts/lib.sh"

# Deploy and smoke must derive the same full revision. A fake live image alone
# cannot catch one side silently returning to a shortened tag. The patterns are
# ANSI-C quoted so the searched-for `${...}` stays literal without escaping.
grep -Fq $'export AGENT_IMAGE_TAG="${source_commit}"' "${scripts_dir}/deploy-gke.sh" ||
	fail "deploy no longer publishes the full source revision as its image tag"
grep -Fq $'source_tag="${head_commit}"' "${scripts_dir}/smoke-gke-model.sh" ||
	fail "smoke no longer expects the full source revision as its image tag"
if grep -Fq -- '--short' "${scripts_dir}/deploy-gke.sh" "${scripts_dir}/smoke-gke-model.sh"; then
	fail "deploy or smoke shortened the source revision"
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${tmp_dir}"' EXIT
mkdir -p "${tmp_dir}/bin"

real_git="$(command -v git)"
export REAL_GIT="${real_git}"
cat >"${tmp_dir}/bin/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ $* == 'status --porcelain' ]]; then
	[[ ${FAKE_GIT_DIRTY:-false} != true ]] || printf ' M infra/kagent/agent.yaml\n'
	exit 0
fi
exec "${REAL_GIT:?}" "$@"
EOF

cat >"${tmp_dir}/bin/tofu" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

output_name="${!#}"
case "${output_name}" in
project_id) printf 'test-project\n' ;;
cluster_name) printf 'test-cluster\n' ;;
cluster_zone) printf 'test-zone\n' ;;
artifact_registry_repository) printf 'test-region-docker.pkg.dev/test-project/agentops\n' ;;
*) exit 64 ;;
esac
EOF

cat >"${tmp_dir}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

arguments=" $* "
if [[ ${arguments} == ' config current-context ' ]]; then
	printf '%s\n' "${FAKE_CONTEXT:-gke_test-project_test-zone_test-cluster}"
elif [[ ${arguments} == *' rollout status '* ]]; then
	exit 0
elif [[ ${arguments} == *' get deployment/agentgateway '* ]]; then
	jq -cn \
		--arg image "${FAKE_GATEWAY_IMAGE:-${FAKE_SOURCE_GATEWAY_IMAGE:?}}" \
		'{
          metadata: {generation: 1},
          status: {observedGeneration: 1},
          spec: {
            template: {
              spec: {
                containers: [{name: "agentgateway", image: $image}],
                volumes: [{name: "config", configMap: {name: "agentgateway-config-test"}}]
              }
            }
          }
        }'
elif [[ ${arguments} == ' kustomize infra/k8s/overlays/gke ' ]]; then
	# The smoke reads the GKE model owners from the render rather than from the base
	# files, because the base carries the open-weight default and the overlay patches
	# the proprietary one in. Emit the two documents it selects from that stream.
	jq -cn --arg model "${FAKE_SOURCE_MODEL:?}" '{
      apiVersion: "kagent.dev/v1alpha2", kind: "Agent",
      metadata: {name: "agentops-agent"},
      spec: {byo: {deployment: {env: [
        {name: "AGENT_MODEL", value: $model},
        {name: "AGENT_A2A_MAX_LLM_CALLS", value: env.FAKE_SOURCE_A2A_MAX_LLM_CALLS}
      ]}}}
    }'
	printf -- '---\n'
	jq -cn --arg model "${FAKE_SOURCE_MODEL:?}" '{
      apiVersion: "kagent.dev/v1alpha2", kind: "ModelConfig",
      metadata: {name: "agentgateway"}, spec: {model: $model}
    }'
elif [[ ${arguments} == *' get configmap/agentgateway-config-test '* ]]; then
	jq -cn --arg config "${FAKE_GATEWAY_CONFIG:?}" '{data: {"config.yaml": $config}}'
elif [[ ${arguments} == *' get agent.kagent.dev/agentops-agent '* ]]; then
	jq -cn \
		--arg model "${FAKE_LIVE_MODEL:-${FAKE_SOURCE_MODEL:?}}" \
		--arg image "${FAKE_AGENT_IMAGE:-${FAKE_EXPECTED_AGENT_IMAGE:?}}" \
		'{
          spec: {
            byo: {
              deployment: {
                image: $image,
                env: [
                  {name: "AGENT_MODEL", value: $model},
                  {name: "AGENT_A2A_MAX_LLM_CALLS", value: env.FAKE_SOURCE_A2A_MAX_LLM_CALLS}
                ]
              }
            }
          }
        }'
elif [[ ${arguments} == *' get deployment/agentops-agent '* ]]; then
	workload_image="${FAKE_WORKLOAD_IMAGE:-${FAKE_AGENT_IMAGE:-${FAKE_EXPECTED_AGENT_IMAGE:?}}}"
	if [[ -n ${FAKE_FINAL_WORKLOAD_IMAGE:-} ]]; then
		deployment_reads=0
		[[ ! -f ${FAKE_DEPLOYMENT_STATE:?} ]] || deployment_reads="$(<"${FAKE_DEPLOYMENT_STATE}")"
		deployment_reads=$((deployment_reads + 1))
		printf '%s\n' "${deployment_reads}" >"${FAKE_DEPLOYMENT_STATE}"
		[[ ${deployment_reads} == 1 ]] || workload_image="${FAKE_FINAL_WORKLOAD_IMAGE}"
	fi
	jq -cn \
		--arg model "${FAKE_LIVE_MODEL:-${FAKE_SOURCE_MODEL:?}}" \
		--arg image "${workload_image}" \
		'{
          metadata: {generation: 1},
          status: {observedGeneration: 1},
          spec: {
            template: {
              spec: {
                containers: [{
                  name: "agentops-agent",
                  image: $image,
                  env: [
                    {name: "AGENT_MODEL", value: $model},
                    {name: "AGENT_A2A_MAX_LLM_CALLS", value: env.FAKE_SOURCE_A2A_MAX_LLM_CALLS}
                  ]
                }]
              }
            }
          }
        }'
elif [[ ${arguments} == *' get modelconfig.kagent.dev/agentgateway '* ]]; then
	jq -cn --arg model "${FAKE_LIVE_MODEL:-${FAKE_SOURCE_MODEL:?}}" '{spec: {model: $model}}'
elif [[ ${arguments} == *' port-forward '* ]]; then
	printf '%s\n' "$$" >"${FAKE_PORT_FORWARD_PID:?}"
	touch "${FAKE_PORT_FORWARD_ACTIVE:?}"
	printf 'Forwarding from 127.0.0.1:31001 -> 3001\n'
	printf 'Forwarding from 127.0.0.1:34000 -> 4000\n'
	printf 'Forwarding from 127.0.0.1:35020 -> 15020\n'
	cleanup() {
		kill -TERM "${sleep_pid:-}" >/dev/null 2>&1 || true
		rm -f -- "${FAKE_PORT_FORWARD_ACTIVE}"
		exit 0
	}
	trap cleanup INT TERM
	sleep 3600 &
	sleep_pid=$!
	wait "${sleep_pid}"
else
	printf 'unexpected kubectl arguments: %s\n' "$*" >&2
	exit 64
fi
EOF

cat >"${tmp_dir}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

payload=""
response_file=""
url=""
increment_model_requests() {
	local increment="$1"
	local current=0

	[[ ! -f ${FAKE_MODEL_REQUEST_COUNT:?} ]] || current="$(<"${FAKE_MODEL_REQUEST_COUNT}")"
	printf '%s\n' "$((current + increment))" >"${FAKE_MODEL_REQUEST_COUNT}"
}
while (($# > 0)); do
	case "$1" in
	--data)
		payload=$2
		shift 2
		;;
	--output)
		response_file=$2
		shift 2
		;;
	--header | --max-time)
		shift 2
		;;
	--fail-with-body | --silent | --show-error)
		shift
		;;
	*)
		url=$1
		shift
		;;
	esac
done
[[ -n ${response_file} ]]

case "${url}" in
http://127.0.0.1:35020/metrics)
	tool_calls=0
	[[ ! -f ${FAKE_MCP_COUNT:?} ]] || tool_calls="$(<"${FAKE_MCP_COUNT}")"
	{
		printf 'agentgateway_mcp_requests_total{method="tools/call",resource_type="tool",resource="get_incident"} %s\n' \
			"${tool_calls}"
		if [[ -f ${FAKE_MODEL_REQUEST_COUNT:?} ]]; then
			printf 'agentgateway_requests_total{protocol="llm",route="llm"} %s\n' \
				"$(<"${FAKE_MODEL_REQUEST_COUNT}")"
		fi
	} >"${response_file}"
	;;
http://127.0.0.1:34000/v1/chat/completions)
	increment_model_requests 1
	request_count=0
	[[ ! -f ${FAKE_CURL_STATE:?} ]] || request_count="$(<"${FAKE_CURL_STATE}")"
	request_count=$((request_count + 1))
	printf '%s\n' "${request_count}" >"${FAKE_CURL_STATE}"

	if [[ ${request_count} == 1 ]]; then
		jq -e --arg expected_model "${FAKE_SOURCE_MODEL:?}" '
          .model == $expected_model
          and (.messages | length) == 1
          and .tool_choice.function.name == "course_probe"
          and .tools[0].function.name == "course_probe"
        ' <<<"${payload}" >/dev/null
		printf '%s\n' '{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-with-signature","type":"function","function":{"name":"course_probe","arguments":"{\"label\":\"gke\"}"}}]}}]}' >"${response_file}"
	else
		jq -e --arg expected_model "${FAKE_SOURCE_MODEL:?}" '
          .model == $expected_model
          and (.messages | length) == 3
          and .messages[1].tool_calls[0].id == "call-with-signature"
          and .messages[2].role == "tool"
          and .messages[2].tool_call_id == "call-with-signature"
          and .messages[2].content == "{\"status\":\"ready\"}"
        ' <<<"${payload}" >/dev/null
		printf '%s\n' '{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"The supplied status is ready."}}]}' >"${response_file}"
	fi
	;;
http://127.0.0.1:31001/)
	increment_model_requests "${FAKE_A2A_MODEL_CALLS:-2}"
	jq -e '
      .method == "message/send"
      and (.id | startswith("gke-model-smoke-"))
      and .params.message.messageId == (.id + "-message")
      and .params.message.role == "user"
      and (.params.message.parts[0].text | contains("Use get_incident exactly once"))
      and (.params.message.parts[0].text | contains("Do not perform any write action"))
    ' <<<"${payload}" >/dev/null
	jq -r '.id' <<<"${payload}" >>"${FAKE_A2A_IDS:?}"
	if [[ ${FAKE_NO_MCP_INCREMENT:-false} != true ]]; then
		tool_calls=0
		[[ ! -f ${FAKE_MCP_COUNT:?} ]] || tool_calls="$(<"${FAKE_MCP_COUNT}")"
		printf '%s\n' "$((tool_calls + 1))" >"${FAKE_MCP_COUNT}"
	fi
	if [[ ${FAKE_A2A_FAIL:-false} == true ]]; then
		printf '%s\n' '{"jsonrpc":"2.0","result":{"kind":"task","status":{"state":"failed"},"metadata":{"adk_error_code":"MODEL_UNAVAILABLE"}}}' >"${response_file}"
	elif [[ ${FAKE_A2A_WRONG_EVIDENCE:-false} == true ]]; then
		printf '%s\n' '{"jsonrpc":"2.0","result":{"kind":"task","status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"text","text":"INC-001 is open"}]}]}}' >"${response_file}"
	else
		printf '%s\n' '{"jsonrpc":"2.0","result":{"kind":"task","status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"text","text":"INC-002 — SEV1 — Inventory service unavailable"}]}]}}' >"${response_file}"
	fi
	;;
*)
	printf 'unexpected URL: %s\n' "${url}" >&2
	exit 64
	;;
esac
EOF
chmod +x "${tmp_dir}/bin/git" "${tmp_dir}/bin/tofu" "${tmp_dir}/bin/kubectl" "${tmp_dir}/bin/curl"

export PATH="${tmp_dir}/bin:${PATH}"
export FAKE_CURL_STATE="${tmp_dir}/request-count"
export FAKE_A2A_IDS="${tmp_dir}/a2a-ids"
export FAKE_MCP_COUNT="${tmp_dir}/mcp-count"
export FAKE_MODEL_REQUEST_COUNT="${tmp_dir}/model-request-count"
export FAKE_PORT_FORWARD_PID="${tmp_dir}/port-forward-pid"
export FAKE_PORT_FORWARD_ACTIVE="${tmp_dir}/port-forward-active"
export FAKE_DEPLOYMENT_STATE="${tmp_dir}/deployment-reads"
export GCP_MODEL_CALL_BUDGET=6
FAKE_GATEWAY_CONFIG="$(<"${repo_dir}/infra/agentgateway/gke/config.yaml")"
export FAKE_GATEWAY_CONFIG
FAKE_SOURCE_TAG="$(git -C "${repo_dir}" rev-parse HEAD)"
export FAKE_SOURCE_TAG
# The GKE plane's model is the overlay patch value, not the base default: the base
# declares the open-weight model so an overlay-free apply cannot select a paid one.
FAKE_SOURCE_MODEL="$(
	yq -er '
      .patches[]
      | select(.target.kind == "ModelConfig")
      | .patch
      | from_yaml
      | .[0].value
    ' "${repo_dir}/infra/k8s/overlays/gke/kustomization.yaml"
)"
export FAKE_SOURCE_MODEL
FAKE_SOURCE_A2A_MAX_LLM_CALLS="$(
	yq -er '
      .spec.byo.deployment.env[]
      | select(.name == "AGENT_A2A_MAX_LLM_CALLS")
      | .value
    ' "${repo_dir}/infra/kagent/agent.yaml"
)"
[[ ${FAKE_SOURCE_A2A_MAX_LLM_CALLS} == 4 ]]
export FAKE_SOURCE_A2A_MAX_LLM_CALLS
FAKE_SOURCE_GATEWAY_IMAGE="$(
	yq -er '
      select(.kind == "Deployment" and .metadata.name == "agentgateway")
      | .spec.template.spec.containers[]
      | select(.name == "agentgateway")
      | .image
    ' "${repo_dir}/infra/k8s/base/agentgateway.yaml"
)"
export FAKE_SOURCE_GATEWAY_IMAGE
FAKE_EXPECTED_AGENT_IMAGE="test-region-docker.pkg.dev/test-project/agentops/agentops-agent:${FAKE_SOURCE_TAG}@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
export FAKE_EXPECTED_AGENT_IMAGE
# Prove the GKE smoke ignores the ambient local-model variable.
export AGENT_MODEL="qwen3:4b-instruct"

head_commit="$(git -C "${repo_dir}" rev-parse HEAD)"
output="$("${scripts_dir}/smoke-gke-model.sh")"
[[ ${output} == "GKE model tool loop and read-only A2A retrieval passed for ${FAKE_SOURCE_MODEL} at ${head_commit}; model requests 4/6" ]]
[[ $(<"${FAKE_CURL_STATE}") == 2 ]]

assert_port_forward_stopped() {
	local port_forward_pid

	[[ ! -e ${FAKE_PORT_FORWARD_ACTIVE} ]] || fail "smoke left its port-forward marked active"
	port_forward_pid="$(<"${FAKE_PORT_FORWARD_PID}")"
	if kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
		fail "smoke left its port-forward process running: ${port_forward_pid}"
	fi
}

assert_port_forward_stopped

rm -f -- "${FAKE_CURL_STATE}"
printf '7\n' >"${FAKE_MCP_COUNT}"
output="$("${scripts_dir}/smoke-gke-model.sh")"
[[ ${output} == "GKE model tool loop and read-only A2A retrieval passed for ${FAKE_SOURCE_MODEL} at ${head_commit}; model requests 4/6" ]]
[[ $(<"${FAKE_MCP_COUNT}") == 8 ]]
assert_port_forward_stopped

expect_protocol_failure() {
	local expected_error="$1"
	local error_file="${tmp_dir}/expected-error"

	if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${error_file}"; then
		fail "smoke unexpectedly passed while expecting: ${expected_error}"
	fi
	if ! grep -Fq -- "${expected_error}" "${error_file}"; then
		cat "${error_file}" >&2
		fail "smoke failed outside the expected boundary: ${expected_error}"
	fi
	[[ -f ${FAKE_CURL_STATE} && $(<"${FAKE_CURL_STATE}") == 2 ]] || {
		cat "${error_file}" >&2
		fail "smoke did not complete both model turns before: ${expected_error}"
	}
	assert_port_forward_stopped
}

rm -f -- "${FAKE_CURL_STATE}"
export FAKE_A2A_FAIL=true
expect_protocol_failure "the A2A investigation did not reach a text-bearing completed task"
unset FAKE_A2A_FAIL

rm -f -- "${FAKE_CURL_STATE}"
export FAKE_A2A_WRONG_EVIDENCE=true
expect_protocol_failure "the A2A investigation omitted seed evidence: INC-002"
unset FAKE_A2A_WRONG_EVIDENCE

rm -f -- "${FAKE_CURL_STATE}"
printf '1\n' >"${FAKE_MCP_COUNT}"
export FAKE_NO_MCP_INCREMENT=true
expect_protocol_failure "get_incident counter delta was not exactly one: before=1, after=1"
unset FAKE_NO_MCP_INCREMENT

unique_a2a_ids="$(sort -u "${FAKE_A2A_IDS}" | wc -l)"
[[ ${unique_a2a_ids} == 5 ]]
FAKE_LIVE_MODEL="qwen3:4b-instruct"
export FAKE_LIVE_MODEL
live_drift_error="${tmp_dir}/live-drift-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${live_drift_error}"; then
	fail "live GKE model drift was accepted"
fi
grep -Fq -- "live Agent model qwen3:4b-instruct does not match source ${FAKE_SOURCE_MODEL}" "${live_drift_error}"
unset FAKE_LIVE_MODEL

FAKE_AGENT_IMAGE="registry.invalid/agentops-agent:${FAKE_SOURCE_TAG}@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
export FAKE_AGENT_IMAGE
agent_image_error="${tmp_dir}/agent-image-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${agent_image_error}"; then
	fail "Agent image outside the exact repository was accepted"
fi
grep -Fq -- "live Agent declaration is outside the exact HEAD-tagged Artifact Registry repository" "${agent_image_error}"
unset FAKE_AGENT_IMAGE

rm -f -- "${FAKE_DEPLOYMENT_STATE}"
FAKE_FINAL_WORKLOAD_IMAGE="test-region-docker.pkg.dev/test-project/agentops/agentops-agent:${FAKE_SOURCE_TAG}@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
export FAKE_FINAL_WORKLOAD_IMAGE
workload_drift_error="${tmp_dir}/workload-drift-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${workload_drift_error}"; then
	fail "post-reconciliation Agent workload drift was accepted"
fi
grep -Fq -- "live Agent declaration and generated workload images disagree" "${workload_drift_error}"
unset FAKE_FINAL_WORKLOAD_IMAGE

FAKE_GATEWAY_IMAGE="cr.agentgateway.dev/agentgateway:wrong@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
export FAKE_GATEWAY_IMAGE
gateway_image_error="${tmp_dir}/gateway-image-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${gateway_image_error}"; then
	fail "gateway image outside the source compatibility pair was accepted"
fi
grep -Fq -- "live agentgateway image ${FAKE_GATEWAY_IMAGE} does not match source ${FAKE_SOURCE_GATEWAY_IMAGE}" "${gateway_image_error}"
unset FAKE_GATEWAY_IMAGE

FAKE_CONTEXT="gke_wrong-project_test-zone_test-cluster"
export FAKE_CONTEXT
context_error="${tmp_dir}/context-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${context_error}"; then
	fail "wrong Kubernetes context was accepted"
fi
grep -Fq -- "kubectl context is ${FAKE_CONTEXT}; expected gke_test-project_test-zone_test-cluster" "${context_error}"
unset FAKE_CONTEXT

export FAKE_GIT_DIRTY=true
dirty_error="${tmp_dir}/dirty-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${dirty_error}"; then
	fail "dirty GKE source was accepted"
fi
grep -Fq -- "GKE smoke requires a clean working tree" "${dirty_error}"
unset FAKE_GIT_DIRTY

rm -f -- "${FAKE_CURL_STATE}" "${FAKE_DEPLOYMENT_STATE}" "${FAKE_MODEL_REQUEST_COUNT}"
printf '0\n' >"${FAKE_MCP_COUNT}"
export FAKE_A2A_MODEL_CALLS=5
model_delta_error="${tmp_dir}/model-delta-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${model_delta_error}"; then
	fail "a provider model-request delta above the approved budget was accepted"
fi
grep -Fq -- "model-request counter delta 7 exceeds GCP_MODEL_CALL_BUDGET 6" "${model_delta_error}"
unset FAKE_A2A_MODEL_CALLS

rm -f -- "${FAKE_CURL_STATE}" "${FAKE_DEPLOYMENT_STATE}" "${FAKE_MODEL_REQUEST_COUNT}"
printf '0\n' >"${FAKE_MCP_COUNT}"
export GCP_MODEL_CALL_BUDGET=5
budget_error="${tmp_dir}/budget-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${budget_error}"; then
	fail "a GCP model-call budget below the six-call worst case was accepted"
fi
grep -Fq -- "GCP_MODEL_CALL_BUDGET 5 is below the smoke worst case 6" "${budget_error}"

rm -f -- "${FAKE_CURL_STATE}" "${FAKE_DEPLOYMENT_STATE}" "${FAKE_MODEL_REQUEST_COUNT}"
printf '0\n' >"${FAKE_MCP_COUNT}"
unset GCP_MODEL_CALL_BUDGET
missing_budget_error="${tmp_dir}/missing-budget-error"
if "${scripts_dir}/smoke-gke-model.sh" >/dev/null 2>"${missing_budget_error}"; then
	fail "a GKE model smoke without GCP_MODEL_CALL_BUDGET was accepted"
fi
grep -Fq -- "GCP_MODEL_CALL_BUDGET must be a positive integer" "${missing_budget_error}"

printf 'GKE model smoke context, payload, retrieval, and fail-closed checks passed\n'
