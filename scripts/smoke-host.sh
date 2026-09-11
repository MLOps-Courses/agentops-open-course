#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd curl model
require_cmd docker gateway
require_cmd go base
require_cmd jq base

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "${script_dir}/.." && pwd)"
readonly repo_root
readonly gateway_wrapper="${repo_root}/infra/scripts/gateway-host.sh"
readonly free_ports="${repo_root}/tools/bin/free-ports"
readonly loopback_relay="${repo_root}/tools/bin/loopback-relay"
readonly curl_image="curlimages/curl:8.21.0@sha256:7c12af72ceb38b7432ab85e1a265cff6ae58e06f95539d539b654f2cfa64bb13"

[[ -x "${free_ports}" && -x "${loopback_relay}" ]] || {
	echo "smoke-host: native repository tools are missing; run 'mise run install' first" >&2
	exit 1
}
allocated_ports="$("${free_ports}" --count 8)"
read -r \
	allocated_fake_port \
	allocated_raw_mcp_port \
	allocated_raw_a2a_port \
	allocated_gateway_mcp_port \
	allocated_gateway_a2a_port \
	allocated_gateway_model_port \
	allocated_gateway_metrics_port \
	allocated_gateway_readiness_port <<<"${allocated_ports}"

readonly fake_port="${AGENTOPS_SMOKE_FAKE_PORT:-${allocated_fake_port}}"
readonly raw_mcp_port="${AGENTOPS_SMOKE_RAW_MCP_PORT:-${allocated_raw_mcp_port}}"
readonly raw_a2a_port="${AGENTOPS_SMOKE_RAW_A2A_PORT:-${allocated_raw_a2a_port}}"
readonly gateway_mcp_port="${AGENTOPS_SMOKE_GATEWAY_MCP_PORT:-${allocated_gateway_mcp_port}}"
readonly gateway_a2a_port="${AGENTOPS_SMOKE_GATEWAY_A2A_PORT:-${allocated_gateway_a2a_port}}"
readonly gateway_model_port="${AGENTOPS_SMOKE_GATEWAY_MODEL_PORT:-${allocated_gateway_model_port}}"
readonly gateway_metrics_port="${AGENTOPS_SMOKE_GATEWAY_METRICS_PORT:-${allocated_gateway_metrics_port}}"
readonly gateway_readiness_port="${AGENTOPS_SMOKE_GATEWAY_READINESS_PORT:-${allocated_gateway_readiness_port}}"
readonly gateway_container="${AGENTOPS_SMOKE_CONTAINER:-agentops-host-smoke-$$}"

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/agentops-host-smoke.XXXXXX")"
readonly work_dir
readonly state_dir="${work_dir}/state"
readonly gateway_runtime_dir="${work_dir}/gateway-runtime"
mkdir -p -- "${state_dir}" "${work_dir}/home" "${work_dir}/tmp"
export NO_PROXY="localhost,127.0.0.1"
export no_proxy="${NO_PROXY}"

declare -a process_ids=()
declare -a gateway_environment=(
	"AGENTOPS_GATEWAY_CONTAINER=${gateway_container}"
	"AGENTOPS_GATEWAY_CONFIG=${repo_root}/infra/agentgateway/host/config.yaml"
	"AGENTOPS_GATEWAY_LOOPBACK_RELAY=auto"
	"AGENTOPS_GATEWAY_RELAY=${loopback_relay}"
	"AGENTOPS_GATEWAY_RUNTIME_DIR=${gateway_runtime_dir}"
	"AGENTOPS_GATEWAY_MCP_PORT=${gateway_mcp_port}"
	"AGENTOPS_GATEWAY_A2A_PORT=${gateway_a2a_port}"
	"AGENTOPS_GATEWAY_MODEL_PORT=${gateway_model_port}"
	"AGENTOPS_GATEWAY_METRICS_PORT=${gateway_metrics_port}"
	"AGENTOPS_GATEWAY_READINESS_PORT=${gateway_readiness_port}"
	"AGENTOPS_MCP_UPSTREAM_PORT=${raw_mcp_port}"
	"AGENTOPS_A2A_UPSTREAM_PORT=${raw_a2a_port}"
	"AGENTOPS_MODEL_UPSTREAM_PORT=${fake_port}"
)

die() {
	echo "smoke-host: $*" >&2
	exit 1
}

validate_port() {
	local name="$1"
	local value="$2"
	local number

	[[ "${value}" =~ ^[0-9]+$ ]] || die "${name} must be an integer, got '${value}'"
	number=$((10#${value}))
	((number >= 1024 && number <= 65535)) ||
		die "${name} must be an unprivileged port between 1024 and 65535, got '${value}'"
}

wait_http() {
	local name="$1"
	local url="$2"
	local log_file="$3"
	local attempt

	# Generous on purpose: a cold CI runner still has to pull the pinned gateway image
	# and open the seed database before the first listener answers.
	for ((attempt = 0; attempt < 480; attempt += 1)); do
		if curl --fail --silent --show-error --max-time 1 "${url}" >/dev/null 2>&1; then
			return
		fi
		sleep 0.25
	done

	echo "${name} did not become ready at ${url}" >&2
	tail -n 80 "${log_file}" >&2 || true
	return 1
}

capture_gateway_logs() {
	local relay_dir="${gateway_runtime_dir}/${gateway_container}/relay"

	if docker container inspect "${gateway_container}" >/dev/null 2>&1; then
		docker container logs "${gateway_container}" >"${work_dir}/gateway.log" 2>&1 || true
	fi
	if [[ -f "${relay_dir}/relay.log" ]]; then
		cp "${relay_dir}/relay.log" "${work_dir}/loopback-relay.log"
	fi
	if [[ -f "${relay_dir}/ready" ]]; then
		cp "${relay_dir}/ready" "${work_dir}/loopback-relay.ready"
	fi
}

dump_failure_logs() {
	local artifact

	for artifact in \
		fake-model.log \
		mcp.log \
		gateway-start.log \
		gateway.log \
		loopback-relay.log \
		a2a.log \
		agent-card.json \
		model-response.json \
		model-request-mask.json \
		model-response-mask.json \
		model-request-reject.json \
		model-response-reject.json \
		a2a-response.json; do
		[[ -f "${work_dir}/${artifact}" ]] || continue
		echo "==> ${artifact} <==" >&2
		tail -n 80 "${work_dir}/${artifact}" >&2 || true
	done
}

stop_processes() {
	local pid
	local running

	for pid in "${process_ids[@]}"; do
		if kill -0 "${pid}" >/dev/null 2>&1; then
			kill -TERM "${pid}" >/dev/null 2>&1 || true
		fi
	done

	for _ in {1..20}; do
		running=0
		for pid in "${process_ids[@]}"; do
			if kill -0 "${pid}" >/dev/null 2>&1; then
				running=1
			fi
		done
		[[ "${running}" == "0" ]] && break
		sleep 0.1
	done

	for pid in "${process_ids[@]}"; do
		if kill -0 "${pid}" >/dev/null 2>&1; then
			kill -KILL "${pid}" >/dev/null 2>&1 || true
		fi
		wait "${pid}" 2>/dev/null || true
	done
}

teardown() {
	local result=0

	capture_gateway_logs
	if ! env "${gateway_environment[@]}" "${gateway_wrapper}" stop >/dev/null 2>&1; then
		result=1
	fi
	stop_processes
	return "${result}"
}

cleanup_on_exit() {
	local result=$?

	trap - EXIT INT TERM
	teardown || true
	if [[ "${result}" == "0" ]]; then
		rm -rf -- "${work_dir}"
	else
		echo "Host smoke failed; logs are preserved at ${work_dir}" >&2
		dump_failure_logs
	fi
	exit "${result}"
}

trap cleanup_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for port_name in \
	fake_port \
	raw_mcp_port \
	raw_a2a_port \
	gateway_mcp_port \
	gateway_a2a_port \
	gateway_model_port \
	gateway_metrics_port \
	gateway_readiness_port; do
	validate_port "${port_name}" "${!port_name}"
done
unique_port_count="$(printf '%s\n' \
	"${fake_port}" \
	"${raw_mcp_port}" \
	"${raw_a2a_port}" \
	"${gateway_mcp_port}" \
	"${gateway_a2a_port}" \
	"${gateway_model_port}" \
	"${gateway_metrics_port}" \
	"${gateway_readiness_port}" | sort -u | wc -l)"
readonly unique_port_count
if ((unique_port_count != 8)); then
	die "smoke ports must be unique"
fi

docker info >/dev/null

# One compiled binary, not `go run`: the surfaces below are killed by pid at teardown, and
# `go run` would leave the compiled child alive under a parent this script had already
# reaped. Building once also keeps the two agent surfaces byte-identical to each other.
readonly agent_binary="${work_dir}/agent"
(cd -- "${repo_root}/agents/go" && CGO_ENABLED=0 go build -o "${agent_binary}" ./cmd/agent)
readonly fake_model_binary="${work_dir}/fake-model"
(cd -- "${repo_root}/tools" && CGO_ENABLED=0 go build -o "${fake_model_binary}" ./cmd/fake-model)

(
	cd -- "${repo_root}"
	exec "${fake_model_binary}" --port "${fake_port}"
) >"${work_dir}/fake-model.log" 2>&1 &
process_ids+=("$!")
wait_http "fake model" "http://127.0.0.1:${fake_port}/healthz" "${work_dir}/fake-model.log"

(
	cd -- "${work_dir}"
	exec env -i \
		HOME="${work_dir}/home" \
		LANG=C \
		LC_ALL=C \
		PATH="${PATH:-/usr/bin:/bin}" \
		TMPDIR="${work_dir}/tmp" \
		AGENT_DATA_DIR="${repo_root}/agents/data" \
		AGENT_STATE_DIR="${state_dir}" \
		MCP_PORT="${raw_mcp_port}" \
		MCP_TRANSPORT=streamable-http \
		OTEL_SDK_DISABLED=true \
		"${agent_binary}" mcp
) >"${work_dir}/mcp.log" 2>&1 &
process_ids+=("$!")
# MCP is a read-only consumer: on fresh shared state it is live but must stay
# unready until the A2A state owner publishes incidents.db below.
wait_http "MCP server liveness" "http://127.0.0.1:${raw_mcp_port}/livez" "${work_dir}/mcp.log"

env "${gateway_environment[@]}" "${gateway_wrapper}" start >"${work_dir}/gateway-start.log" 2>&1
wait_http "gateway metrics" "http://localhost:${gateway_metrics_port}/metrics" "${work_dir}/gateway-start.log"
wait_http "gateway readiness" "http://localhost:${gateway_readiness_port}/healthz/ready" "${work_dir}/gateway-start.log"

kernel="$(uname -s)"
docker_os="$(docker info --format '{{.OperatingSystem}}')"
# Native Linux needs a relay for the gateway's loopback-bound MCP, A2A, and
# model upstreams. Metrics flow directly from Prometheus to the gateway container.
if [[ "${kernel}" == "Linux" && "${docker_os}" != *"Docker Desktop"* ]]; then
	relay_ready="${gateway_runtime_dir}/${gateway_container}/relay/ready"
	[[ -f "${relay_ready}" ]] || die "native Linux gateway started without its bridge-only loopback relay"
	relay_listen_host="$(awk -F= '$1 == "listen_host" { print $2 }' "${relay_ready}")"
	relay_ports="$(awk -F= '$1 == "ports" { print $2 }' "${relay_ready}")"
	# The relay binds the wrapper's OWN network, not the shared default bridge. That is the
	# whole point of the dedicated network: on the default bridge every unrelated container on
	# the host could reach the learner's MCP, A2A, and Ollama ports while the gateway ran.
	gateway_network="${AGENTOPS_GATEWAY_NETWORK:-${gateway_container}-net}"
	network_gateway="$(docker network inspect "${gateway_network}" --format '{{(index .IPAM.Config 0).Gateway}}')"
	[[ "${relay_listen_host}" == "${network_gateway}" ]]
	[[ "${relay_listen_host}" != "0.0.0.0" && "${relay_listen_host}" != "127.0.0.1" ]]
	# Prove the isolation claim rather than assuming it: the default bridge must NOT be where
	# the relay listens, or the wrapper has silently regressed to the shared address.
	default_bridge_gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || true)"
	[[ -z "${default_bridge_gateway}" || "${relay_listen_host}" != "${default_bridge_gateway}" ]]
	[[ ",${relay_ports}," != *",${gateway_metrics_port},"* ]]
fi

(
	cd -- "${work_dir}"
	exec env -i \
		HOME="${work_dir}/home" \
		LANG=C \
		LC_ALL=C \
		PATH="${PATH:-/usr/bin:/bin}" \
		TMPDIR="${work_dir}/tmp" \
		AGENT_A2A_HOST=localhost \
		AGENT_A2A_PORT="${raw_a2a_port}" \
		AGENT_A2A_STREAMING=false \
		AGENT_DATA_DIR="${repo_root}/agents/data" \
		AGENT_MCP_URL="http://localhost:${gateway_mcp_port}/mcp" \
		AGENT_MODEL=qwen3:4b-instruct \
		AGENT_MODEL_PROVIDER=openai-compatible \
		AGENT_PII_MODEL_BASE_URL="http://localhost:${fake_port}/v1" \
		AGENT_SEMANTIC_RETRIEVAL=false \
		AGENT_STATE_DIR="${state_dir}" \
		OPENAI_API_KEY=local-ollama \
		OPENAI_BASE_URL="http://localhost:${gateway_model_port}/v1" \
		OTEL_SDK_DISABLED=true \
		"${agent_binary}" a2a
) >"${work_dir}/a2a.log" 2>&1 &
process_ids+=("$!")
wait_http "A2A server" "http://127.0.0.1:${raw_a2a_port}/healthz" "${work_dir}/a2a.log"
wait_http "MCP server readiness" "http://127.0.0.1:${raw_mcp_port}/healthz" "${work_dir}/mcp.log"
wait_http "gateway A2A" "http://localhost:${gateway_a2a_port}/healthz" "${work_dir}/gateway.log"

# The governed MCP surface is checked with a real MCP client over the gateway, and the
# expected tool set is asked of the agent's own allowlist rather than repeated here, so a
# widened server or a widened gateway rule fails instead of quietly passing.
cat >"${work_dir}/mcp-client-check.go" <<'GO'
package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "agentops-host-smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: os.Getenv("MCP_URL")}, nil)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", os.Getenv("MCP_URL"), err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("listing tools: %w", err)
	}
	offered := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		offered = append(offered, tool.Name)
	}
	expected := compose.MCPReadToolNames()
	slices.Sort(offered)
	slices.Sort(expected)
	if !slices.Equal(offered, expected) {
		return fmt.Errorf("unexpected MCP tools: got %v, want %v", offered, expected)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_incidents"})
	if err != nil {
		return fmt.Errorf("calling list_incidents: %w", err)
	}
	if result.IsError {
		return fmt.Errorf("list_incidents failed: %v", result.Content)
	}
	if len(result.Content) == 0 {
		return fmt.Errorf("list_incidents returned no content")
	}
	return nil
}
GO
(
	cd -- "${repo_root}/agents/go"
	exec env -i \
		HOME="${work_dir}/home" \
		LANG=C \
		LC_ALL=C \
		NO_PROXY="${NO_PROXY}" \
		PATH="${PATH:-/usr/bin:/bin}" \
		TMPDIR="${work_dir}/tmp" \
		GOCACHE="${HOME}/.cache/go-build" \
		GOMODCACHE="${HOME}/go/pkg/mod" \
		MCP_URL="http://localhost:${gateway_mcp_port}/mcp" \
		go run "${work_dir}/mcp-client-check.go"
)

# The agent speaks only the Responses API, so this is the exact path the gateway
# routes as `responses` (infra/agentgateway/host/config.yaml `policies.ai.routes`).
curl --fail --silent --show-error \
	-H "Authorization: Bearer local-ollama" \
	-H "Content-Type: application/json" \
	--data '{"model":"qwen3:4b-instruct","input":"Say hello.","stream":false}' \
	"http://localhost:${gateway_model_port}/v1/responses" \
	>"${work_dir}/model-response.json"
model_content="$(jq -r '[.output[] | select(.type == "message") | .content[] | select(.type == "output_text") | .text] | first' "${work_dir}/model-response.json")"
model_name="$(jq -r '.model' "${work_dir}/model-response.json")"
[[ "${model_content}" == "Fake model response for platform latency measurement." ]]
[[ "${model_name}" == "qwen3:4b-instruct" ]]

# The second governed client shape on the same route. The evaluation judge
# (evals/judge.go) speaks chat-completions and .env.example tells the reader to point
# it at :4000, so this asserts what that instruction promises. It is not a duplicate of
# the Responses probe above: when the route carried only `responses`, this request
# still answered 200 with an empty Responses body and no `choices`, which the judge
# reported as "gateway judge returned no verdict content" — a green transport probe
# hiding a dead evaluation. Read `choices[0].message.content`, the exact field the
# judge parses, so the shape and not merely the status is pinned.
curl --fail --silent --show-error \
	-H "Authorization: Bearer local-ollama" \
	-H "Content-Type: application/json" \
	--data '{"model":"qwen3:4b-instruct","messages":[{"role":"user","content":"Say hello."}]}' \
	"http://localhost:${gateway_model_port}/v1/chat/completions" \
	>"${work_dir}/model-chat-completion.json"
chat_object="$(jq -r '.object' "${work_dir}/model-chat-completion.json")"
chat_model="$(jq -r '.model' "${work_dir}/model-chat-completion.json")"
chat_content="$(jq -r '.choices[0].message.content' "${work_dir}/model-chat-completion.json")"
[[ "${chat_object}" == "chat.completion" ]]
[[ "${chat_model}" == "qwen3:4b-instruct" ]]
[[ "${chat_content}" == "${model_content}" ]]

# Routing the judge here is only worth doing if the judge is governed like the agent,
# so prove the prompt guard fires on this shape too rather than assuming the policy
# stack applies to every route type the route admits.
chat_reject_status="$(
	curl --silent --show-error \
		-H "Authorization: Bearer local-ollama" \
		-H "Content-Type: application/json" \
		--output "${work_dir}/model-chat-reject.json" \
		--write-out '%{http_code}' \
		--data '{"model":"qwen3:4b-instruct","messages":[{"role":"user","content":"reject-probe@example.invalid"}]}' \
		"http://localhost:${gateway_model_port}/v1/chat/completions"
)"
[[ "${chat_reject_status}" == "400" ]]
grep -Fq "Request rejected by the course prompt guard." "${work_dir}/model-chat-reject.json"

# An endpoint the route does not declare must not become a hole through the gateway
# to the model host: the parse stage refuses it before any upstream call.
chat_unrouted_status="$(
	curl --silent --show-error \
		-H "Authorization: Bearer local-ollama" \
		-H "Content-Type: application/json" \
		--output /dev/null \
		--write-out '%{http_code}' \
		--data '{"model":"qwen3:4b-instruct","prompt":"Say hello."}' \
		"http://localhost:${gateway_model_port}/v1/completions"
)"
[[ "${chat_unrouted_status}" == "503" ]]

# Keep the historical rejection canaries while proving that ordinary email is
# centrally masked on both sides of the model boundary. The fake model's probe
# response makes pass-through distinguishable without logging the request body.
request_reject_status="$(
	curl --silent --show-error \
		-H "Authorization: Bearer local-ollama" \
		-H "Content-Type: application/json" \
		--output "${work_dir}/model-request-reject.json" \
		--write-out '%{http_code}' \
		--data '{"model":"qwen3:4b-instruct","input":"reject-probe@example.invalid","stream":false}' \
		"http://localhost:${gateway_model_port}/v1/responses"
)"
[[ "${request_reject_status}" == "400" ]]
grep -Fq "Request rejected by the course prompt guard." "${work_dir}/model-request-reject.json"

curl --fail --silent --show-error \
	-H "Authorization: Bearer local-ollama" \
	-H "Content-Type: application/json" \
	--data '{"model":"qwen3:4b-instruct","input":"request-mask-probe request-mask@example.test","stream":false}' \
	"http://localhost:${gateway_model_port}/v1/responses" \
	>"${work_dir}/model-request-mask.json"
request_mask_content="$(jq -r '[.output[] | select(.type == "message") | .content[] | select(.type == "output_text") | .text] | first' "${work_dir}/model-request-mask.json")"
[[ "${request_mask_content}" == "FAKE_MODEL_SAW_MASKED_REQUEST_PII" ]]

curl --fail --silent --show-error \
	-H "Authorization: Bearer local-ollama" \
	-H "Content-Type: application/json" \
	--data '{"model":"qwen3:4b-instruct","input":"response-mask-probe","stream":false}' \
	"http://localhost:${gateway_model_port}/v1/responses" \
	>"${work_dir}/model-response-mask.json"
response_mask_content="$(jq -r '[.output[] | select(.type == "message") | .content[] | select(.type == "output_text") | .text] | first' "${work_dir}/model-response-mask.json")"
[[ "${response_mask_content}" == Contact* ]]
[[ "${response_mask_content}" != *"response-mask@example.test"* ]]

response_reject_status="$(
	curl --silent --show-error \
		-H "Authorization: Bearer local-ollama" \
		-H "Content-Type: application/json" \
		--output "${work_dir}/model-response-reject.json" \
		--write-out '%{http_code}' \
		--data '{"model":"qwen3:4b-instruct","input":"response-reject-probe","stream":false}' \
		"http://localhost:${gateway_model_port}/v1/responses"
)"
[[ "${response_reject_status}" == "502" ]]
grep -Fq "Model response rejected by the course data-loss guard." "${work_dir}/model-response-reject.json"

curl --fail --silent --show-error \
	"http://localhost:${gateway_a2a_port}/.well-known/agent-card.json" \
	>"${work_dir}/agent-card.json"
agent_name="$(jq -r '.name' "${work_dir}/agent-card.json")"
agent_url="$(jq -r '.supportedInterfaces[0].url' "${work_dir}/agent-card.json")"
agent_protocol="$(jq -r '.supportedInterfaces[0].protocolBinding' "${work_dir}/agent-card.json")"
[[ "${agent_name}" == "AgentOps Agent" ]]
[[ "${agent_url}" == "http://localhost:${gateway_a2a_port}/" ]]
[[ "${agent_protocol}" == "JSONRPC" ]]

curl --fail --silent --show-error \
	-H "Content-Type: application/json" \
	--data '{
		"jsonrpc": "2.0",
		"id": "host-smoke",
		"method": "message/send",
		"params": {
			"message": {
				"kind": "message",
				"role": "user",
				"messageId": "host-smoke-message",
				"parts": [{"kind": "text", "text": "Reply with one short sentence."}]
			}
		}
	}' \
	"http://localhost:${gateway_a2a_port}/" \
	>"${work_dir}/a2a-response.json"
jq -e --arg expected "${model_content}" '
		.error == null
		and (
			(
				.result.kind == "message"
				and any(.result.parts[]?; .kind == "text" and .text == $expected)
			)
			or (
				.result.kind == "task"
				and .result.status.state == "completed"
				and ((.result.metadata.adk_error_code // "") == "")
				and (
					any(.result.artifacts[]?.parts[]?; .kind == "text" and .text == $expected)
					or any(.result.status.message.parts[]?; .kind == "text" and .text == $expected)
				)
			)
		)
	' "${work_dir}/a2a-response.json" >/dev/null

allowed_cors_status="$(
	curl --silent --show-error \
		--dump-header "${work_dir}/cors-allowed.headers" \
		--output /dev/null \
		--write-out '%{http_code}' \
		-X OPTIONS \
		-H "Origin: http://localhost:8001" \
		-H "Access-Control-Request-Method: POST" \
		-H "Access-Control-Request-Headers: content-type" \
		"http://localhost:${gateway_a2a_port}/"
)"
[[ "${allowed_cors_status}" =~ ^2[0-9][0-9]$ ]]
tr -d '\r' <"${work_dir}/cors-allowed.headers" >"${work_dir}/cors-allowed.normalized"
allowed_origin="$(awk 'tolower($1) == "access-control-allow-origin:" { print $2 }' "${work_dir}/cors-allowed.normalized")"
[[ "${allowed_origin}" == "http://localhost:8001" ]]

curl --silent --show-error \
	--dump-header "${work_dir}/cors-denied.headers" \
	--output /dev/null \
	-X OPTIONS \
	-H "Origin: http://evil.invalid" \
	-H "Access-Control-Request-Method: POST" \
	"http://localhost:${gateway_a2a_port}/"
tr -d '\r' <"${work_dir}/cors-denied.headers" >"${work_dir}/cors-denied.normalized"
if awk 'tolower($1) == "access-control-allow-origin:" { found = 1 } END { exit found ? 0 : 1 }' "${work_dir}/cors-denied.normalized"; then
	die "gateway returned an Access-Control-Allow-Origin header for a denied origin"
fi

curl --fail --silent --show-error \
	"http://localhost:${gateway_metrics_port}/metrics" \
	>"${work_dir}/gateway-metrics.txt"
grep -Eq '^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^}]*\})? [0-9]' "${work_dir}/gateway-metrics.txt"
# The fake response reports deterministic token usage. An exact catalog lookup
# proves agentgateway attributed those tokens to the configured provider/model;
# the zero cost is the explicit local provider fee, not an invoice estimate.
cost_lookup_count="$(awk '
  /^agentgateway_cost_catalog_lookups_total\{/ &&
  /status="Exact"/ &&
  /gen_ai_system="openai"/ &&
  /gen_ai_request_model="qwen3:4b-instruct"/ { total += $NF }
  END { print total + 0 }
' "${work_dir}/gateway-metrics.txt")"
((cost_lookup_count > 0)) ||
	die "gateway metrics did not record an exact qwen3:4b-instruct cost-catalog lookup"
awk '
  /^agentgateway_gen_ai_client_cost_usd_total\{/ &&
  /gen_ai_system="openai"/ &&
  /gen_ai_request_model="qwen3:4b-instruct"/ &&
  ($NF + 0) == 0 { found = 1 }
  END { exit found ? 0 : 1 }
' "${work_dir}/gateway-metrics.txt" ||
	die "gateway metrics did not attribute zero provider cost to qwen3:4b-instruct"
# Prometheus runs on the wrapper-owned bridge, so prove its exact direct target
# rather than a different host-published or relay path.
docker run --rm \
	--network "${AGENTOPS_GATEWAY_NETWORK:-${gateway_container}-net}" \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges=true \
	"${curl_image}" \
	--fail --silent --show-error --max-time 5 \
	"http://agentops-gateway:15020/metrics" \
	>"${work_dir}/gateway-metrics-from-container.txt"
grep -Eq '^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^}]*\})? [0-9]' "${work_dir}/gateway-metrics-from-container.txt"
curl --fail --silent --show-error \
	"http://localhost:${gateway_readiness_port}/healthz/ready" \
	>"${work_dir}/gateway-readiness.txt"
read -r readiness_body <"${work_dir}/gateway-readiness.txt"
[[ "${readiness_body}" == "ready" ]]
env "${gateway_environment[@]}" "${gateway_wrapper}" status | grep -Fq "status=running"

teardown
if docker container inspect "${gateway_container}" >/dev/null 2>&1; then
	die "managed gateway container still exists after teardown"
fi
for pid in "${process_ids[@]}"; do
	if kill -0 "${pid}" >/dev/null 2>&1; then
		die "host smoke process ${pid} is still running after teardown"
	fi
done

trap - EXIT INT TERM
rm -rf -- "${work_dir}"
echo "Host smoke passed: fake model, MCP, A2A, agentgateway, CORS, readiness, host/container metrics, and teardown."
