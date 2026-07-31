#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

profile=${1:-base}
support_file="${lib_dir}/../SUPPORT.md"
capacity_contract="$(sed -n 's/^<!-- local-platform-capacity: \(.*\) -->$/\1/p' "${support_file}")"
if [[ ! ${capacity_contract} =~ ^total-ram-gib=([0-9]+)[[:space:]]+free-disk-gib=([0-9]+)$ ]]; then
	fail "SUPPORT.md must declare one valid local-platform-capacity contract"
fi
readonly platform_total_ram_gib="${BASH_REMATCH[1]}"
readonly platform_free_disk_gib="${BASH_REMATCH[2]}"

# Each entry is "<command> <install task>": a missing tool names the one command
# that materializes it instead of a single generic sentence for the whole tier.
required=()

add_tier() {
	local install_task="$1"
	local tool
	shift

	for tool in "$@"; do
		required+=("${tool} ${install_task}")
	done
}

# Base is everything the base-tier gates shell out to, not only the three tools
# that author a page: check:data needs sqlite3, check:links lychee, check:shell
# shfmt + shellcheck, check:workflows actionlint, check:licenses jq.
add_base_tier() {
	add_tier "mise run install" git uv dprint sqlite3 jq lychee shfmt shellcheck actionlint
}

# openssl is the gateway tier's only host-provided tool: gateway:host:auth mints
# the demo TLS material and JWTs with it.
add_gateway_tier() {
	add_tier "mise run install:platform" curl docker openssl yq
}

add_platform_tier() {
	add_gateway_tier
	add_tier "mise run install:platform" \
		rg k3d kubectl helm helmfile skaffold kubeconform kube-linter agentgateway sops age-keygen
}

add_gcp_tier() {
	add_gateway_tier
	add_tier "mise run install:platform" \
		rg kubectl helm helmfile skaffold kubeconform tofu tflint
	add_tier "mise run install:gcp" gcloud gke-gcloud-auth-plugin
}

report_linux_capacity() {
	local available_kib disk_available_kib total_kib

	if [[ -r /proc/meminfo ]]; then
		total_kib=$(awk '$1 == "MemTotal:" { print $2 }' /proc/meminfo)
		available_kib=$(awk '$1 == "MemAvailable:" { print $2 }' /proc/meminfo)
		awk \
			-v available="${available_kib}" \
			-v planned="${platform_total_ram_gib}" \
			-v total="${total_kib}" \
			'BEGIN {
				printf "capacity   %.1f GiB total RAM; %.1f GiB currently available", total / 1048576, available / 1048576
				if (total < planned * 1048576) {
					printf " (below the %s GiB local-platform planning value)", planned
				}
				printf "\n"
			}'
	fi

	disk_available_kib=$(df -Pk . | awk 'NR == 2 { print $4 }')
	awk \
		-v available="${disk_available_kib}" \
		-v planned="${platform_free_disk_gib}" \
		'BEGIN {
			printf "capacity   %.1f GiB free disk", available / 1048576
			if (available < planned * 1048576) {
				printf " (below the %s GiB local-platform planning value)", planned
			}
			printf "\n"
		}'
}

case "${profile}" in
base)
	add_base_tier
	;;
model)
	add_base_tier
	add_tier "mise run install" curl ollama
	;;
gateway)
	add_base_tier
	add_gateway_tier
	;;
platform)
	add_base_tier
	add_platform_tier
	;;
gcp)
	add_base_tier
	add_gcp_tier
	;;
*)
	printf 'usage: %s {base|model|gateway|platform|gcp}\n' "$0" >&2
	exit 2
	;;
esac

missing=0
for entry in "${required[@]}"; do
	command_name="${entry%% *}"
	install_task="${entry#* }"
	if ! command -v "${command_name}" >/dev/null 2>&1; then
		printf 'missing: %-24s install it with: %s\n' "${command_name}" "${install_task}" >&2
		missing=1
	fi
done

if ((missing)); then
	printf '\nRun the commands above, then re-run: mise run doctor:%s\n' "${profile}" >&2
	exit 1
fi

for python_environment in .venv/bin/python agents/python/.venv/bin/python; do
	if [[ ! -x ${python_environment} ]]; then
		fail "${python_environment} missing; run mise run install"
	fi
done

printf '%-10s ready\n' "${profile}"

if [[ -f .env ]]; then
	printf 'env        .env available to explicit live/config tasks\n'
else
	printf 'env        optional .env is absent\n'
fi

if [[ ${profile} == model ]]; then
	if ! curl --fail --silent --show-error http://127.0.0.1:11434/api/tags |
		jq -e '.models[]?.name | startswith("qwen3:4b-instruct")' >/dev/null; then
		fail 'ollama     start Ollama and run: ollama pull qwen3:4b-instruct'
	fi
	printf 'ollama     qwen3:4b-instruct ready on 127.0.0.1:11434\n'
fi

case "${profile}" in
gateway | platform | gcp)
	[[ -x infra/scripts/gateway-host.sh ]] || fail 'gateway    wrapper is not executable'
	docker info >/dev/null 2>&1 || fail 'docker     daemon is unavailable'
	docker compose version >/dev/null
	printf 'docker     ready\n'
	;;
*) ;;
esac

if [[ ${profile} == platform ]]; then
	require_cgroup_v2 /sys/fs/cgroup
	printf 'cgroup     v2 ready\n'
	report_linux_capacity

	if ! helm plugin list | rg -q '^diff[[:space:]]+3\.15\.10'; then
		fail 'helm       helm-diff 3.15.10 is missing; run mise run install:platform'
	fi
	printf 'helm       helm-diff 3.15.10 ready\n'

	context=$(kubectl config current-context 2>/dev/null || true)
	if [[ ${context} == "k3d-local" ]]; then
		printf 'cluster    k3d-local selected\n'
	elif [[ -n ${context} ]]; then
		printf 'cluster    %s selected; local tasks require k3d-local\n' "${context}"
	else
		printf 'cluster    not created yet; run mise run cluster:start when needed\n'
	fi
fi

if [[ ${profile} == gcp ]]; then
	if ! helm plugin list | rg -q '^diff[[:space:]]+3\.15\.10'; then
		fail 'helm       helm-diff 3.15.10 is missing; run mise run install:platform'
	fi
	printf 'helm       helm-diff 3.15.10 ready\n'

	project="${GCP_PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
	if [[ -n ${project} ]]; then
		gcloud projects describe "${project}" --format='value(projectId)' >/dev/null
		billing_enabled="$(gcloud billing projects describe "${project}" --format='value(billingEnabled)')"
		[[ ${billing_enabled} == True ]] ||
			fail "gcp        billing is not enabled for project ${project}"
		printf 'gcp        project %s is active and billing-enabled\n' "${project}"
	else
		fail 'gcp        set GCP_PROJECT_ID or select a billing-enabled active project'
	fi
	if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
		fail 'gcp        ADC unavailable; run gcloud auth application-default login'
	fi
	printf 'gcp        Application Default Credentials and GKE kubectl authentication ready\n'
fi
