#!/usr/bin/env bash

# Start the project-scoped Compose stack and remove its containers if any
# readiness or hardening check fails. Named volumes survive for the next run.

scripts_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${scripts_dir}/../.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${repo_dir}/scripts/lib.sh"

require_cmd docker gateway

compose=(
	docker compose
	--project-name agentops-observability
	--file "${repo_dir}/infra/observability/compose.yaml"
)
readonly -a compose

cleanup_failed_start() {
	local status=$?
	if ((status != 0)); then
		"${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
	fi
	exit "${status}"
}
trap cleanup_failed_start EXIT

"${compose[@]}" up --build --detach --wait
if [[ ${OBSERVABILITY_FORCE_READINESS_FAILURE:-false} == true ]]; then
	fail "injected observability readiness failure"
fi
(
	cd "${repo_dir}" || exit
	./scripts/check-observability.sh ready
)

trap - EXIT
