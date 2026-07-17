#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "${script_dir}/.." && pwd)"
readonly repo_root
readonly gateway_wrapper="${repo_root}/infra/scripts/gateway-host.sh"
readonly agent_python="${repo_root}/agents/python/.venv/bin/python"
readonly curl_image="curlimages/curl:8.21.0@sha256:7c12af72ceb38b7432ab85e1a265cff6ae58e06f95539d539b654f2cfa64bb13"

[[ -x "${agent_python}" ]] || {
	echo "smoke-host: agent environment is missing; run 'mise run install' first" >&2
	exit 1
}
allocated_ports="$(
	"${agent_python}" - <<'PY'
import socket

sockets = [socket.socket() for _ in range(8)]
try:
    for listener in sockets:
        listener.bind(("127.0.0.1", 0))
    print(*(listener.getsockname()[1] for listener in sockets))
finally:
    for listener in sockets:
        listener.close()
PY
)"
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
	"AGENTOPS_GATEWAY_PYTHON=${agent_python}"
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

	# The reference agent imports ADK, Presidio, and spaCy. A cold CI runner can
	# spend tens of seconds loading those modules before Uvicorn binds.
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
"${agent_python}" - \
	"${fake_port}" \
	"${raw_mcp_port}" \
	"${raw_a2a_port}" \
	"${gateway_mcp_port}" \
	"${gateway_a2a_port}" \
	"${gateway_model_port}" \
	"${gateway_metrics_port}" \
	"${gateway_readiness_port}" <<'PY'
import socket
import sys

ports = [int(value) for value in sys.argv[1:]]
if len(set(ports)) != len(ports):
    raise SystemExit(f"smoke ports must be unique: {ports}")
for port in ports:
    with socket.socket() as listener:
        listener.bind(("0.0.0.0", port))
PY

docker info >/dev/null

(
	cd -- "${repo_root}"
	exec "${agent_python}" load/fake_model.py --port "${fake_port}"
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
		"${agent_python}" -m agent.mcp_server
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
if [[ "${kernel}" == "Linux" && "${docker_os}" != *"Docker Desktop"* ]]; then
	relay_ready="${gateway_runtime_dir}/${gateway_container}/relay/ready"
	[[ -f "${relay_ready}" ]] || die "native Linux gateway started without its bridge-only loopback relay"
	relay_listen_host="$(awk -F= '$1 == "listen_host" { print $2 }' "${relay_ready}")"
	relay_ports="$(awk -F= '$1 == "ports" { print $2 }' "${relay_ready}")"
	bridge_gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
	[[ "${relay_listen_host}" == "${bridge_gateway}" ]]
	[[ "${relay_listen_host}" != "0.0.0.0" && "${relay_listen_host}" != "127.0.0.1" ]]
	[[ ",${relay_ports}," == *",${gateway_metrics_port},"* ]]
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
		AGENT_SEMANTIC_RETRIEVAL=false \
		AGENT_STATE_DIR="${state_dir}" \
		OPENAI_API_KEY=local-ollama \
		OPENAI_BASE_URL="http://localhost:${gateway_model_port}/v1" \
		OTEL_SDK_DISABLED=true \
		"${agent_python}" -m agent.server
) >"${work_dir}/a2a.log" 2>&1 &
process_ids+=("$!")
wait_http "A2A server" "http://127.0.0.1:${raw_a2a_port}/healthz" "${work_dir}/a2a.log"
wait_http "MCP server readiness" "http://127.0.0.1:${raw_mcp_port}/healthz" "${work_dir}/mcp.log"
wait_http "gateway A2A" "http://localhost:${gateway_a2a_port}/healthz" "${work_dir}/gateway.log"

env -i \
	HOME="${work_dir}/home" \
	LANG=C \
	LC_ALL=C \
	NO_PROXY="${NO_PROXY}" \
	PATH="${PATH:-/usr/bin:/bin}" \
	TMPDIR="${work_dir}/tmp" \
	MCP_URL="http://localhost:${gateway_mcp_port}/mcp" \
	"${agent_python}" - <<'PY'
import asyncio
import os

from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

EXPECTED_TOOLS = {
    "get_incident",
    "get_runbook",
    "get_service_status",
    "list_incidents",
    "search_runbooks",
    "search_service_logs",
}


async def main() -> None:
    async with streamable_http_client(os.environ["MCP_URL"], terminate_on_close=False) as (read, write, _):
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools = await session.list_tools()
            names = {tool.name for tool in tools.tools}
            if names != EXPECTED_TOOLS:
                raise RuntimeError(f"unexpected MCP tools: {sorted(names)}")
            result = await session.call_tool("list_incidents", {})
            if result.isError:
                raise RuntimeError(f"list_incidents failed: {result.content}")
            if not result.content:
                raise RuntimeError("list_incidents returned no content")


asyncio.run(main())
PY

curl --fail --silent --show-error \
	-H "Authorization: Bearer local-ollama" \
	-H "Content-Type: application/json" \
	--data '{"model":"qwen3:4b-instruct","messages":[{"role":"user","content":"Say hello."}],"stream":false}' \
	"http://localhost:${gateway_model_port}/v1/chat/completions" \
	>"${work_dir}/model-response.json"
model_content="$(jq -r '.choices[0].message.content' "${work_dir}/model-response.json")"
model_name="$(jq -r '.model' "${work_dir}/model-response.json")"
[[ "${model_content}" == "Fake model response for platform latency measurement." ]]
[[ "${model_name}" == "qwen3:4b-instruct" ]]

curl --fail --silent --show-error \
	"http://localhost:${gateway_a2a_port}/.well-known/agent-card.json" \
	>"${work_dir}/agent-card.json"
agent_name="$(jq -r '.name' "${work_dir}/agent-card.json")"
agent_url="$(jq -r '.url' "${work_dir}/agent-card.json")"
[[ "${agent_name}" == "AgentOps Agent" ]]
[[ "${agent_url}" == "http://localhost:${gateway_a2a_port}" ]]

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
jq -e '.error == null and (.result.kind == "task" or .result.kind == "message")' "${work_dir}/a2a-response.json" >/dev/null

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
# Prometheus runs in Compose, so prove the same target from a container. On
# native Linux this crosses the wrapper-owned bridge relay; Docker Desktop
# provides its own host.docker.internal transport.
docker run --rm \
	--network bridge \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges=true \
	--add-host host.docker.internal:host-gateway \
	"${curl_image}" \
	--fail --silent --show-error --max-time 5 \
	"http://host.docker.internal:${gateway_metrics_port}/metrics" \
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
