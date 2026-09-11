#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

requirements_only=false
if [[ ${1:-} == --requirements ]]; then
	requirements_only=true
	shift
fi
profile=${1:-base}

# Each entry pairs a command with its honest remedy: a repository task for
# managed tools, or a reviewed host source for prerequisites mise does not own.
required=()

# --8<-- [start:doctor-tool-tiers]
readonly -a base_managed_tools=(go hugo golangci-lint gotestsum dprint sqlite3 jq lychee shfmt shellcheck actionlint zizmor rg trivy)
readonly -a base_host_tools=(git curl cc install make tar)
readonly -a model_host_tools=(ollama)
readonly -a gateway_managed_tools=(yq)
readonly -a gateway_host_tools=(docker openssl)
readonly -a platform_tools=(
	k3d kubectl helm helmfile skaffold kubeconform kube-linter agentgateway promtool sops age-keygen
)
readonly -a gcp_platform_tools=(kubectl helm helmfile skaffold kubeconform tofu tflint)
readonly -a gcp_host_tools=(gcloud gke-gcloud-auth-plugin)
# --8<-- [end:doctor-tool-tiers]

add_tier() {
	local remedy="$1"
	local tool
	shift

	for tool in "$@"; do
		required+=("${tool}"$'\t'"${remedy}")
	done
}

# Base is everything the base-tier gates shell out to, not only the three tools
# that author a page: check:data needs sqlite3, check:links lychee, check:shell
# shfmt + shellcheck, check:workflows actionlint + Zizmor, and check:licenses reads the font
# license with rg before Trivy owns the dependency verdict. rg is listed here rather than left
# to the platform tier because check:licenses:core needs it: without it the base doctor reported
# ready and check:core then failed on a tool no learner had been told to install. The heavier
# tiers no longer repeat it — every profile adds this tier first, and nothing deduplicates
# "${required[@]}", so a second entry only made one missing tool print twice, once per remedy.
add_base_tier() {
	add_tier "mise run install" "${base_managed_tools[@]}"
	add_tier "install Git from a reviewed host package source" "${base_host_tools[0]}"
	add_tier "install it from a reviewed host package source" "${base_host_tools[@]:1}"
}

# openssl is the gateway tier's only host-provided tool: gateway:host:auth mints
# the demo TLS material and JWTs with it.
add_gateway_tier() {
	add_tier "mise run install:platform" "${gateway_managed_tools[@]}"
	add_tier "follow 1.2. Container Engine to install a supported container engine" "${gateway_host_tools[0]}"
	add_tier "install OpenSSL from a reviewed host package source" "${gateway_host_tools[1]}"
}

add_platform_tier() {
	add_gateway_tier
	add_tier "mise run install:platform" "${platform_tools[@]}"
}

add_gcp_tier() {
	add_gateway_tier
	add_tier "mise run install:platform" "${gcp_platform_tools[@]}"
	add_tier "install the Google Cloud CLI from a reviewed host package source" "${gcp_host_tools[0]}"
	add_tier "install it from the same reviewed Cloud SDK or host package source" "${gcp_host_tools[1]}"
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
	add_tier "follow 1.4. Providers to install Ollama" "${model_host_tools[@]}"
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

if [[ ${requirements_only} == true ]]; then
	printf '%s\n' "${required[@]}"
	exit 0
fi

support_file="${lib_dir}/../SUPPORT.md"
capacity_contract="$(sed -n 's/^<!-- local-platform-capacity: \(.*\) -->$/\1/p' "${support_file}")"
if [[ ! ${capacity_contract} =~ ^total-ram-gib=([0-9]+)[[:space:]]+free-disk-gib=([0-9]+)$ ]]; then
	fail "SUPPORT.md must declare one valid local-platform-capacity contract"
fi
readonly platform_total_ram_gib="${BASH_REMATCH[1]}"
readonly platform_free_disk_gib="${BASH_REMATCH[2]}"
helm_diff_version="$("${lib_dir}/install-helm-diff.sh" --version)"
readonly helm_diff_version

missing=0
for entry in "${required[@]}"; do
	IFS=$'\t' read -r command_name remedy <<<"${entry}"
	if ! command -v "${command_name}" >/dev/null 2>&1; then
		printf 'missing: %-24s remedy: %s\n' "${command_name}" "${remedy}" >&2
		missing=1
	fi
done

if ((missing)); then
	printf '\nRun the commands above, then re-run: mise run doctor:%s\n' "${profile}" >&2
	exit 1
fi

for native_tool in tools/bin/course-certificate tools/bin/course-evidence tools/bin/loopback-relay tools/bin/static-server; do
	if [[ ! -x ${native_tool} ]]; then
		fail "${native_tool} missing; run mise run install"
	fi
done

printf '%-10s ready\n' "${profile}"

if [[ -f .env ]]; then
	printf 'env        .env available to explicit live/config tasks\n'
else
	printf 'env        optional .env is absent\n'
fi

if [[ ${profile} == model ]]; then
	# The Go agent reaches Ollama through ADK's model/openaimodel, which speaks
	# only the OpenAI Responses API. Ollama grew /v1/responses in 0.13.3, so an
	# older daemon 404s every model call however healthy it otherwise looks.
	# Workstream M ran the tool-calling and human-approval behaviours against
	# 0.13.3 and confirmed the floor holds there.
	# --8<-- [start:doctor-ollama-floor]
	readonly ollama_minimum_version=0.13.3

	ollama_version="$(curl --fail --silent --show-error http://127.0.0.1:11434/api/version |
		jq -r '.version // empty')" ||
		fail 'ollama     start Ollama: it is not answering on 127.0.0.1:11434'
	[[ -n ${ollama_version} ]] || fail 'ollama     /api/version returned no version'

	# sort -V orders versions; the floor stays first only when it is not newer.
	ollama_versions="$(printf '%s\n%s\n' "${ollama_minimum_version}" "${ollama_version}" | sort -V)"
	ollama_oldest="${ollama_versions%%$'\n'*}"
	if [[ ${ollama_oldest} != "${ollama_minimum_version}" ]]; then
		fail "ollama     ${ollama_version} is older than the ${ollama_minimum_version} floor; the Responses API is missing"
	fi
	# --8<-- [end:doctor-ollama-floor]

	if ! curl --fail --silent --show-error http://127.0.0.1:11434/api/tags |
		jq -e '.models[]?.name | startswith("qwen3:4b-instruct")' >/dev/null; then
		fail 'ollama     start Ollama and run: ollama pull qwen3:4b-instruct'
	fi
	printf 'ollama     %s with qwen3:4b-instruct ready on 127.0.0.1:11434\n' "${ollama_version}"

	# A tag list is not an inference. A stale `ollama serve` whose install tree had
	# been deleted answered /api/version and /api/tags correctly while every model
	# call failed in under a second, and this profile reported `ready`. So ask the
	# provider to actually infer, and print how long the smallest possible call
	# took: that number is what a learner needs before starting Chapter 2, because
	# one grounded turn costs several calls of this size or larger.
	inference_started="${SECONDS}"
	if ! curl --fail --silent --show-error \
		--max-time "${DOCTOR_MODEL_PROBE_TIMEOUT_S:-120}" \
		--header 'content-type: application/json' \
		--data '{"model":"qwen3:4b-instruct","input":"Reply with exactly: NONE","max_output_tokens":16}' \
		http://127.0.0.1:11434/v1/responses |
		jq -e '.status == "completed"' >/dev/null; then
		fail 'ollama     answers /api/tags but cannot infer; restart Ollama and re-pull qwen3:4b-instruct'
	fi
	printf 'inference  ok in %ss (a full turn needs several calls of this size or larger)\n' \
		"$((SECONDS - inference_started))"
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

	if ! helm plugin list | awk -v expected="${helm_diff_version}" '$1 == "diff" && $2 == expected { found=1 } END { exit !found }'; then
		fail "helm       helm-diff ${helm_diff_version} is missing; run mise run install:platform"
	fi
	printf 'helm       helm-diff %s ready\n' "${helm_diff_version}"

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
	if ! helm plugin list | awk -v expected="${helm_diff_version}" '$1 == "diff" && $2 == expected { found=1 } END { exit !found }'; then
		fail "helm       helm-diff ${helm_diff_version} is missing; run mise run install:platform"
	fi
	printf 'helm       helm-diff %s ready\n' "${helm_diff_version}"

	project="${GCP_PROJECT_ID:-}"
	[[ ${project} =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] ||
		fail 'gcp        set the explicitly approved GCP_PROJECT_ID; ambient project fallback is disabled'
	ambient_project="$(gcloud config get-value project 2>/dev/null || true)"
	if [[ -n ${ambient_project} && ${ambient_project} != '(unset)' && ${ambient_project} != "${project}" ]]; then
		fail "gcp        ambient project ${ambient_project} disagrees with approved GCP_PROJECT_ID ${project}"
	fi
	active_account="$(gcloud auth list --filter=status:ACTIVE --format='value(account)')"
	[[ -n ${active_account} && ${active_account} != *$'\n'* ]] ||
		fail 'gcp        expected exactly one active gcloud principal'
	project_record="$(gcloud projects describe "${project}" --project="${project}" --format=json)"
	resolved_project="$(jq -er '.projectId' <<<"${project_record}")"
	project_number="$(jq -er '.projectNumber | tostring | select(test("^[1-9][0-9]{5,19}$"))' <<<"${project_record}")"
	[[ ${resolved_project} == "${project}" && -n ${project_number} ]] ||
		fail 'gcp        the approved project ID and resolved project number could not be verified'
	billing_enabled="$(
		gcloud billing projects describe "${project}" \
			--project="${project}" \
			--format='value(billingEnabled)'
	)"
	[[ ${billing_enabled} == True ]] ||
		fail "gcp        billing is not enabled for project ${project}"
	printf 'gcp        project %s is active and billing-enabled\n' "${project}"
	if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
		fail 'gcp        ADC unavailable; run gcloud auth application-default login'
	fi
	printf 'gcp        Application Default Credentials and GKE kubectl authentication ready\n'
fi
