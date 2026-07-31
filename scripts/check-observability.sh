#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd docker gateway
require_cmd curl
require_cmd jq base

mode=${1:-ready}
case "${mode}" in
ready | stopped) ;;
*) fail "usage: $0 [ready|stopped]" ;;
esac

compose=(
	docker compose
	--project-name agentops-observability
	--file infra/observability/compose.yaml
)
readonly -a compose

if [[ ${mode} == stopped ]]; then
	project_containers=$("${compose[@]}" ps --all --quiet)
	if [[ -n ${project_containers} ]]; then
		fail "agentops-observability still owns containers after teardown"
	fi
	printf 'observability teardown complete: no project container remains\n'
	exit 0
fi

expected_services=$(
	printf '%s\n' alertmanager grafana loki mlflow otel-collector prometheus | sort
)
running_services=$("${compose[@]}" ps --status running --services | sort)
if [[ ${running_services} != "${expected_services}" ]]; then
	printf 'expected running services:\n%s\nactual running services:\n%s\n' \
		"${expected_services}" \
		"${running_services}" >&2
	exit 1
fi

wait_http() {
	local label=$1
	local url=$2
	local attempt=0
	while ((attempt < 60)); do
		((attempt += 1))
		if curl --fail --silent --show-error --max-time 2 "${url}" >/dev/null 2>&1; then
			printf '%s ready: %s\n' "${label}" "${url}"
			return
		fi
		sleep 1
	done
	fail "${label} did not become ready: ${url}"
}

wait_http MLflow "http://127.0.0.1:${MLFLOW_PORT:-5000}/health"
wait_http Loki "http://127.0.0.1:${LOKI_PORT:-3100}/ready"
wait_http Prometheus "http://127.0.0.1:${PROMETHEUS_PORT:-9090}/-/ready"
wait_http Alertmanager "http://127.0.0.1:${ALERTMANAGER_PORT:-9093}/-/ready"
wait_http Grafana "http://127.0.0.1:${GRAFANA_PORT:-3002}/api/health"

collector_status=$(
	curl \
		--silent \
		--show-error \
		--max-time 2 \
		--output /dev/null \
		--write-out '%{http_code}' \
		--header 'Content-Type: application/json' \
		--data '{"resourceSpans":[]}' \
		"http://127.0.0.1:${OTEL_HTTP_PORT:-4318}/v1/traces"
)
[[ ${collector_status} == 200 ]]
printf 'OpenTelemetry Collector ready: OTLP/HTTP accepted an empty valid batch\n'

grafana_database=$(
	curl --fail --silent --show-error \
		"http://127.0.0.1:${GRAFANA_PORT:-3002}/api/health" |
		jq -r '.database'
)
[[ ${grafana_database} == ok ]]

container_ids=$("${compose[@]}" ps --quiet)
while IFS= read -r container_id; do
	[[ -n ${container_id} ]] || continue
	container_name=$(docker inspect --format '{{.Name}}' "${container_id}")
	container_user=$(docker inspect --format '{{.Config.User}}' "${container_id}")
	readonly_root=$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "${container_id}")
	cap_drop=$(docker inspect --format '{{json .HostConfig.CapDrop}}' "${container_id}")
	security_opt=$(docker inspect --format '{{json .HostConfig.SecurityOpt}}' "${container_id}")
	memory_limit=$(docker inspect --format '{{.HostConfig.Memory}}' "${container_id}")
	pids_limit=$(docker inspect --format '{{.HostConfig.PidsLimit}}' "${container_id}")

	[[ -n ${container_user} && ${container_user%%:*} != 0 ]]
	[[ ${readonly_root} == true ]]
	jq -e 'index("ALL") != null' <<<"${cap_drop}" >/dev/null
	jq -e 'map(startswith("no-new-privileges")) | any' <<<"${security_opt}" >/dev/null
	[[ ${memory_limit} -gt 0 ]]
	[[ ${pids_limit} -gt 0 ]]
	printf '%s hardened: non-root, read-only, no-new-privileges, bounded\n' "${container_name#/}"
done <<<"${container_ids}"

for mapping in \
	"mlflow 5000" \
	"loki 3100" \
	"otel-collector 4317" \
	"otel-collector 4318" \
	"alertmanager 9093" \
	"prometheus 9090" \
	"grafana 3000"; do
	read -r service container_port <<<"${mapping}"
	published=$("${compose[@]}" port "${service}" "${container_port}")
	[[ ${published} == 127.0.0.1:* ]]
done
printf 'observability exposure remains loopback-only\n'
