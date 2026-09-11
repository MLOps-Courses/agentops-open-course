#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${script_dir}/../.." && pwd)"
fixture_parent="$(realpath -e -- "${TMPDIR:-/tmp}")"
fixture_dir="$(mktemp -d "${fixture_parent}/agentops-platform-backup-drill.XXXXXX")"
readonly script_dir repo_dir fixture_parent fixture_dir

cleanup() {
	[[ -d ${fixture_dir} && ! -L ${fixture_dir} ]] || {
		echo "refusing to clean invalid backup-drill fixtures: ${fixture_dir}" >&2
		return 1
	}
	[[ $(dirname -- "${fixture_dir}") == "${fixture_parent}" ]] || {
		echo "refusing to clean backup-drill fixtures outside ${fixture_parent}" >&2
		return 1
	}
	[[ $(basename -- "${fixture_dir}") =~ ^agentops-platform-backup-drill\.[A-Za-z0-9]{6}$ ]] || {
		echo "refusing to clean unexpected backup-drill fixtures: ${fixture_dir}" >&2
		return 1
	}
	rm -r -- "${fixture_dir:?}"
}
trap cleanup EXIT

mkdir -m 700 -- "${fixture_dir}/bin"

cat >"${fixture_dir}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

context=""
namespace=""
declare -a args=()
while (($# > 0)); do
	case "$1" in
	--context)
		context="${2:-}"
		shift 2
		;;
	--context=*)
		context="${1#--context=}"
		shift
		;;
	--namespace | -n)
		namespace="${2:-}"
		shift 2
		;;
	--namespace=*)
		namespace="${1#--namespace=}"
		shift
		;;
	*)
		args+=("$1")
		shift
		;;
	esac
done

printf '%s\t%s\t' "${context}" "${namespace}" >>"${FAKE_KUBECTL_LOG:?}"
printf '%q ' "${args[@]}" >>"${FAKE_KUBECTL_LOG:?}"
printf '\n' >>"${FAKE_KUBECTL_LOG:?}"

if [[ -z ${context} || ${context} != "${FAKE_ALLOWED_CONTEXT:?}" ]]; then
	echo "unknown explicit context: ${context:-<empty>}" >&2
	exit 64
fi

state_file="${FAKE_STATE_DIR:?}/replicas.json"
resource_dir="${FAKE_STATE_DIR:?}/resources"

resource_path() {
	local kind="$1"
	local name="$2"
	printf '%s/%s--%s.json\n' "${resource_dir}" "${kind}" "${name}"
}

maybe_fail() {
	local boundary="$1"
	if [[ ${FAKE_FAIL_AFTER:-} == "${boundary}" ]]; then
		printf '%s\n' "${boundary}" >>"${FAKE_MUTATION_LOG:?}"
		exit 91
	fi
}

write_replica() {
	local field="$1"
	local value="$2"
	local temporary="${state_file}.tmp"
	jq --arg field "${field}" --argjson value "${value}" \
		'.[$field] = $value' "${state_file}" >"${temporary}"
	mv -- "${temporary}" "${state_file}"
}

get_output=""
for ((index = 0; index < ${#args[@]}; index++)); do
	if [[ ${args[index]} == -o || ${args[index]} == --output ]]; then
		get_output="${args[index + 1]:-}"
	elif [[ ${args[index]} == -o=* || ${args[index]} == --output=* ]]; then
		get_output="${args[index]#*=}"
	fi
done

case "${args[0]:-}" in
config)
	if [[ ${args[1]:-} == get-contexts ]]; then
		printf '%s\n' "${FAKE_RESOLVED_CONTEXT:-${FAKE_ALLOWED_CONTEXT}}"
	else
		echo "unexpected kubectl config command: ${args[*]}" >&2
		exit 64
	fi
	;;
get)
	resource="${args[1]:-}"
	name="${args[2]:-}"
	case "${resource}/${name}" in
	namespace/agentops)
		printf 'namespace/agentops\n'
		;;
	cronjob/agentops-state-backup)
		printf 'cronjob.batch/agentops-state-backup\n'
		;;
	agent/agentops-agent)
		agent_replicas="$(jq -c '.agent_cr' "${state_file}")"
		jq -cn --argjson replicas "${agent_replicas}" \
			'{spec:{byo:{deployment:{replicas:$replicas}}}}'
		;;
	deployment/agentops-agent)
		agent_replicas="$(jq -c '.agent_deployment' "${state_file}")"
		jq -cn --argjson replicas "${agent_replicas}" '{spec:{replicas:$replicas}}'
		;;
	deployment/agentops-mcp)
		mcp_replicas="$(jq -c '.mcp_deployment' "${state_file}")"
		jq -cn --argjson replicas "${mcp_replicas}" '{spec:{replicas:$replicas}}'
		;;
	pods/*)
		if [[ ${get_output} == name ]]; then
			agent_replicas="$(jq -r '.agent_deployment' "${state_file}")"
			mcp_replicas="$(jq -r '.mcp_deployment' "${state_file}")"
			if [[ ${args[*]} == *agentops-agent* && ${agent_replicas} != 0 ]]; then
				printf 'pod/agentops-agent-pod\n'
			elif [[ ${args[*]} == *agentops-mcp* && ${mcp_replicas} != 0 ]]; then
				printf 'pod/agentops-mcp-pod\n'
			fi
		else
			jq -cn --arg image "${FAKE_AGENT_IMAGE:?}" '{items:[{
			  metadata:{name:"agentops-agent-pod"},
			  status:{phase:"Running",containerStatuses:[{name:"agent",ready:true,imageID:("docker-pullable://"+$image)}]},
			  spec:{containers:[{name:"agent",image:$image}]}
			}]}'
		fi
		;;
	pod/agentops-agent-pod)
		jq -cn --arg image "${FAKE_AGENT_IMAGE:?}" \
			'{status:{containerStatuses:[{name:"agent",imageID:("docker-pullable://"+$image)}]}}'
		;;
	job/* | configmap/*)
		kind="Job"
		[[ ${resource} == configmap ]] && kind="ConfigMap"
		path="$(resource_path "${kind}" "${name}")"
		if [[ -f ${path} ]]; then
			if [[ ${kind} == Job ]]; then
				jq '.status.conditions = [{"type":"Complete","status":"True"}]' "${path}"
			else
				cat -- "${path}"
			fi
		fi
		;;
	*)
		echo "unexpected kubectl get: ${args[*]}" >&2
		exit 64
		;;
	esac
	;;
exec)
	if [[ ${args[*]} == *'platform-backup seed'* ]]; then
		marker=""
		for ((index = 0; index < ${#args[@]}; index++)); do
			[[ ${args[index]} != --marker ]] || marker="${args[index + 1]}"
		done
		jq -cn --arg marker "${marker}" '{
		  audit_action:"restart_service",
		  audit_invocation_id:$marker,
		  audit_target:"inventory",
		  memory_incident_id:"INC-002",
		  memory_note:($marker+"-long-term-note"),
		  memory_user_id:"platform-ci",
		  session_id:"session-1",
		  task_id:"task-1"
		}'
	elif [[ ${args[*]} == *'/app/agent version'* ]]; then
		printf '%s\n' "${FAKE_VERSION_JSON:?}"
	else
		echo "unexpected kubectl exec: ${args[*]}" >&2
		exit 64
	fi
	;;
create)
	if [[ ${args[1]:-} == configmap && ${args[*]} == *--dry-run=client* ]]; then
		jq -cn --arg name "${args[2]}" '{apiVersion:"v1",kind:"ConfigMap",metadata:{name:$name,namespace:"agentops"}}'
	elif [[ ${args[1]:-} == job && ${args[*]} == *--dry-run=client* ]]; then
		jq -cn --arg name "${args[3]}" --arg image "${FAKE_AGENT_IMAGE:?}" '{
		  apiVersion:"batch/v1",kind:"Job",metadata:{name:$name,namespace:"agentops"},
		  spec:{template:{spec:{containers:[{name:"backup",image:$image}],volumes:[{name:"backups"}]}}}
		}'
	elif [[ ${args[1]:-} == --filename || ${args[1]:-} == -f ]]; then
		body="$(cat)"
		kind="$(jq -er '.kind' <<<"${body}")"
		name="$(jq -er '.metadata.name' <<<"${body}")"
		path="$(resource_path "${kind}" "${name}")"
		printf '%s\n' "${body}" >"${path}"
		printf 'create\t%s\t%s\n' "${kind}" "${name}" >>"${FAKE_MUTATION_LOG:?}"
		case "${kind}/${name}" in
		ConfigMap/platform-backup-evidence-*) maybe_fail configmap-created ;;
		Job/platform-state-backup-*) maybe_fail backup-job-created ;;
		Job/platform-state-restore-*) maybe_fail restore-job-created ;;
		esac
	else
		echo "unexpected kubectl create: ${args[*]}" >&2
		exit 64
	fi
	;;
patch)
	[[ ${args[1]:-}/${args[2]:-} == agent/agentops-agent ]]
	patch=""
	for ((index = 0; index < ${#args[@]}; index++)); do
		[[ ${args[index]} != --patch ]] || patch="${args[index + 1]}"
	done
	value="$(jq -c '.spec.byo.deployment.replicas' <<<"${patch}")"
	write_replica agent_cr "${value}"
	printf 'patch\tagent\tagentops-agent\t%s\n' "${value}" >>"${FAKE_MUTATION_LOG:?}"
	[[ ${value} != 0 ]] || maybe_fail agent-scaled
	;;
scale)
	[[ ${args[1]:-} == deployment ]]
	name="${args[2]:-}"
	replicas=""
	for argument in "${args[@]}"; do
		[[ ${argument} != --replicas=* ]] || replicas="${argument#--replicas=}"
	done
	case "${name}" in
	agentops-agent)
		write_replica agent_deployment "${replicas}"
		printf 'scale\tdeployment\t%s\t%s\n' "${name}" "${replicas}" >>"${FAKE_MUTATION_LOG:?}"
		[[ ${replicas} != 0 ]] || maybe_fail agent-deployment-scaled
		;;
	agentops-mcp)
		write_replica mcp_deployment "${replicas}"
		printf 'scale\tdeployment\t%s\t%s\n' "${name}" "${replicas}" >>"${FAKE_MUTATION_LOG:?}"
		[[ ${replicas} != 0 ]] || maybe_fail mcp-scaled
		;;
	*) exit 64 ;;
	esac
	;;
logs)
	if [[ ${args[*]} == *manifest-reader* ]]; then
		printf '%s\n' "${FAKE_MANIFEST_JSON:?}"
	elif [[ ${args[*]} == *platform-state-restore-* ]]; then
		printf 'platform backup restore drill verified\n'
	else
		printf 'backup completed\n'
	fi
	;;
delete)
	kind="${args[1]:-}"
	name="${args[2]:-}"
	canonical_kind="Job"
	[[ ${kind} != configmap ]] || canonical_kind="ConfigMap"
	path="$(resource_path "${canonical_kind}" "${name}")"
	if [[ ${FAKE_CLEANUP_FAIL:-} == backup-job && ${canonical_kind}/${name} == Job/platform-state-backup-* ]]; then
		echo "synthetic backup Job cleanup failure" >&2
		exit 73
	fi
	if [[ -f ${path} ]]; then
		rm -- "${path}"
		printf 'delete\t%s\t%s\n' "${canonical_kind}" "${name}" >>"${FAKE_MUTATION_LOG:?}"
	fi
	;;
rollout)
	[[ ${args[1]:-} == status ]]
	;;
*)
	echo "unexpected kubectl command: ${args[*]}" >&2
	exit 64
	;;
esac
EOF

cat >"${fixture_dir}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

url="${!#}"
[[ ${url} == "${FAKE_AGENT_CARD_URL:?}" ]] || {
	echo "unexpected curl URL: ${url}" >&2
	exit 64
}
printf '%s\n' "${FAKE_AGENT_CARD_JSON:?}"
EOF

cat >"${fixture_dir}/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

[[ ${DOCKER_CONFIG:-} == "${FAKE_DOCKER_CONFIG:?}" ]] || {
	echo "docker did not receive the explicit config boundary" >&2
	exit 64
}
[[ -z ${DOCKER_AUTH_CONFIG:-} ]] || {
	echo "ambient Docker auth reached the drill" >&2
	exit 64
}
case "${1:-}" in
pull)
	[[ ${2:-} == "${FAKE_AGENT_IMAGE:?}" ]]
	printf '%s\n' "${FAKE_AGENT_IMAGE}" >"${FAKE_DOCKER_IMAGE_STATE:?}"
	printf 'pull\t%s\n' "${FAKE_AGENT_IMAGE}" >>"${FAKE_DOCKER_LOG:?}"
	[[ ${FAKE_FAIL_AFTER:-} != image-pulled ]] || exit 91
	;;
image)
	case "${2:-}" in
	inspect)
		[[ ${!#} == "${FAKE_AGENT_IMAGE:?}" ]]
		[[ -f ${FAKE_DOCKER_IMAGE_STATE:?} && $(<"${FAKE_DOCKER_IMAGE_STATE}") == "${FAKE_AGENT_IMAGE}" ]] || exit 1
		if [[ $* == *--format* ]]; then
			printf '%s\n' "${FAKE_LABELS_JSON:?}"
		fi
		;;
	rm)
		[[ ${3:-} == "${FAKE_AGENT_IMAGE:?}" ]]
		rm -f -- "${FAKE_DOCKER_IMAGE_STATE:?}"
		printf 'remove\t%s\n' "${FAKE_AGENT_IMAGE}" >>"${FAKE_DOCKER_LOG:?}"
		;;
	*) exit 64 ;;
	esac
	;;
*) exit 64 ;;
esac
EOF

chmod +x "${fixture_dir}/bin/kubectl" "${fixture_dir}/bin/curl" "${fixture_dir}/bin/docker"

readonly revision=5dd9c33494a37928a0f4ebe66ec57d0081f7d541
readonly tree_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly created=2026-08-08T15:16:25Z
readonly version=1.0.0
readonly reader_image=curlimages/curl:8.21.0@sha256:7c12af72ceb38b7432ab85e1a265cff6ae58e06f95539d539b654f2cfa64bb13
readonly agent_image=registry.localhost:5050/agentops-agent@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly agent_card_url=http://127.0.0.1:8080/.well-known/agent-card.json

export PATH="${fixture_dir}/bin:${PATH}"
export FAKE_ALLOWED_CONTEXT=k3d-local
export FAKE_AGENT_IMAGE="${agent_image}"
export FAKE_AGENT_CARD_URL="${agent_card_url}"
export FAKE_AGENT_CARD_JSON="{\"name\":\"AgentOps Agent\",\"version\":\"${version}\"}"
export FAKE_VERSION_JSON="{\"mode\":\"release\",\"version\":\"${version}\",\"source_identity\":\"${revision}\",\"revision\":\"${revision}\",\"tree_digest\":\"${tree_digest}\",\"build_timestamp\":\"${created}\",\"dirty\":false}"
export FAKE_LABELS_JSON="{\"org.opencontainers.image.created\":\"${created}\",\"org.opencontainers.image.version\":\"${version}\",\"org.opencontainers.image.revision\":\"${revision}\",\"dev.fmind.agentops.build-mode\":\"release\",\"dev.fmind.agentops.source-identity\":\"${revision}\",\"dev.fmind.agentops.source-revision\":\"${revision}\",\"dev.fmind.agentops.source-tree-digest\":\"${tree_digest}\",\"dev.fmind.agentops.source-dirty\":\"false\"}"
export FAKE_MANIFEST_JSON="{\"source\":{\"application\":\"agentops-agent\",\"mode\":\"release\",\"version\":\"${version}\",\"source_identity\":\"${revision}\",\"commit\":\"${revision}\",\"revision\":\"${revision}\",\"tree_digest\":\"${tree_digest}\",\"build_timestamp\":\"${created}\",\"dirty\":false},\"databases\":[{\"filename\":\"runtime.db\"}]}"

export AGENT_BUILD_MODE=release
export AGENT_SOURCE_COMMIT="${revision}"
export AGENT_SOURCE_REVISION="${revision}"
export AGENT_SOURCE_TREE_DIGEST="${tree_digest}"
export AGENT_SOURCE_DIRTY=false
export OCI_CREATED="${created}"
export OCI_VERSION="${version}"
export DOCKER_AUTH_CONFIG='{"auths":{"ambient.invalid":{"auth":"not-used"}}}'

new_case() {
	local name="$1"
	local case_dir="${fixture_dir}/${name}"
	mkdir -m 700 -- "${case_dir}" "${case_dir}/work" "${case_dir}/work/docker" "${case_dir}/state" "${case_dir}/state/resources"
	printf '%s\n' '{"auths":{}}' >"${case_dir}/work/docker/config.json"
	chmod 600 "${case_dir}/work/docker/config.json"
	printf '%s\n' '{"agent_cr":3,"agent_deployment":2,"mcp_deployment":4}' >"${case_dir}/state/replicas.json"
	printf '%s\n' '{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unrelated-job","labels":{"agentops.fmind.dev/backup-drill-id":"someone-else"}}}' \
		>"${case_dir}/state/resources/Job--unrelated-job.json"
	: >"${case_dir}/kubectl.log"
	: >"${case_dir}/mutations.log"
	: >"${case_dir}/docker.log"
	printf '%s\n' "${case_dir}"
}

set_case_environment() {
	local case_dir="$1"
	export FAKE_STATE_DIR="${case_dir}/state"
	export FAKE_KUBECTL_LOG="${case_dir}/kubectl.log"
	export FAKE_MUTATION_LOG="${case_dir}/mutations.log"
	export FAKE_DOCKER_CONFIG="${case_dir}/work/docker"
	export FAKE_DOCKER_IMAGE_STATE="${case_dir}/docker-image-state"
	export FAKE_DOCKER_LOG="${case_dir}/docker.log"
	unset FAKE_RESOLVED_CONTEXT
}

declare -a drill_arguments=()
build_drill_args() {
	local case_dir="$1"
	drill_arguments=(
		--context k3d-local
		--work-dir "${case_dir}/work"
		--source-revision "${revision}"
		--reader-image "${reader_image}"
		--agent-card-url "${agent_card_url}"
		--docker-config "${case_dir}/work/docker"
		--evidence-marker platform-backup-canary
	)
}

run_drill() {
	local case_dir="$1"
	local stdout_file="$2"
	local stderr_file="$3"
	build_drill_args "${case_dir}"
	"${script_dir}/platform-backup-drill.sh" "${drill_arguments[@]}" >"${stdout_file}" 2>"${stderr_file}"
}

assert_no_cluster_mutation() {
	local case_dir="$1"
	[[ ! -s ${case_dir}/mutations.log ]] || {
		echo "invalid input mutated the fake cluster: $(<"${case_dir}/mutations.log")" >&2
		exit 1
	}
}

assert_restored_and_owned_resources_removed() {
	local case_dir="$1"
	local expected_image_state="${2:-absent}"
	local created_resources
	local deleted_resources

	created_resources="$(awk -F '\t' '$1 == "create" {print $2 "/" $3}' "${case_dir}/mutations.log" | LC_ALL=C sort)"
	deleted_resources="$(awk -F '\t' '$1 == "delete" {print $2 "/" $3}' "${case_dir}/mutations.log" | LC_ALL=C sort)"
	[[ -n ${created_resources} && ${created_resources} == "${deleted_resources}" ]] || {
		echo "temporary resource create/delete sets differ" >&2
		printf 'created:\n%s\ndeleted:\n%s\n' "${created_resources}" "${deleted_resources}" >&2
		exit 1
	}
	jq -e '.agent_cr == 3 and .agent_deployment == 2 and .mcp_deployment == 4' \
		"${case_dir}/state/replicas.json" >/dev/null
	[[ -f ${case_dir}/state/resources/Job--unrelated-job.json ]]
	if [[ ${expected_image_state} == present ]]; then
		[[ -f ${case_dir}/docker-image-state && $(<"${case_dir}/docker-image-state") == "${agent_image}" ]] || {
			echo "a pre-existing image was removed during cleanup" >&2
			exit 1
		}
	else
		[[ ! -e ${case_dir}/docker-image-state ]] || {
			echo "a drill-pulled image survived cleanup" >&2
			exit 1
		}
	fi
	if compgen -G "${case_dir}/state/resources/Job--platform-*" >/dev/null ||
		compgen -G "${case_dir}/state/resources/ConfigMap--platform-*" >/dev/null; then
		echo "owned temporary resources survived cleanup" >&2
		ls -1 "${case_dir}/state/resources" >&2
		exit 1
	fi
}

# Ambient legacy variables never satisfy the explicit interface.
ambient_case="$(new_case ambient)"
set_case_environment "${ambient_case}"
export BACKUP_EVIDENCE_MARKER=ambient-marker GITHUB_SHA="${revision}" CURL_IMAGE="${reader_image}" RUNNER_TEMP="${ambient_case}/work"
if "${script_dir}/platform-backup-drill.sh" >"${ambient_case}/stdout" 2>"${ambient_case}/stderr"; then
	echo "ambient backup-drill arguments unexpectedly passed" >&2
	exit 1
fi
assert_no_cluster_mutation "${ambient_case}"

assert_rejected_before_mutation() {
	local name="$1"
	shift
	local case_dir
	case_dir="$(new_case "${name}")"
	set_case_environment "${case_dir}"
	if "${script_dir}/platform-backup-drill.sh" "$@" >"${case_dir}/stdout" 2>"${case_dir}/stderr"; then
		echo "${name} unexpectedly passed" >&2
		exit 1
	fi
	assert_no_cluster_mutation "${case_dir}"
}

argument_template_case="$(new_case argument-template)"
build_drill_args "${argument_template_case}"
valid_args=("${drill_arguments[@]}")

symlink_case="$(new_case symlink-work-dir)"
ln -s -- "${symlink_case}/work" "${symlink_case}/linked-work"
set_case_environment "${symlink_case}"
declare -a symlink_args=()
build_drill_args "${symlink_case}"
symlink_args=("${drill_arguments[@]}")
symlink_args[3]="${symlink_case}/linked-work"
if "${script_dir}/platform-backup-drill.sh" "${symlink_args[@]}" >"${symlink_case}/stdout" 2>"${symlink_case}/stderr"; then
	echo "symlink work directory unexpectedly passed" >&2
	exit 1
fi
assert_no_cluster_mutation "${symlink_case}"

image_pull_case="$(new_case fail-image-pulled)"
set_case_environment "${image_pull_case}"
export FAKE_FAIL_AFTER=image-pulled
if run_drill "${image_pull_case}" "${image_pull_case}/stdout" "${image_pull_case}/stderr"; then
	echo "failure after introducing the Docker image unexpectedly passed" >&2
	exit 1
fi
unset FAKE_FAIL_AFTER
assert_no_cluster_mutation "${image_pull_case}"
[[ ! -e ${image_pull_case}/docker-image-state ]]
grep -Fq $'pull\t' "${image_pull_case}/docker.log"
grep -Fq $'remove\t' "${image_pull_case}/docker.log"

bad_revision_args=("${valid_args[@]}")
bad_revision_args[5]=deadbeef
assert_rejected_before_mutation bad-revision "${bad_revision_args[@]}"

mutable_image_args=("${valid_args[@]}")
mutable_image_args[7]=curlimages/curl:8.21.0
assert_rejected_before_mutation mutable-reader-image "${mutable_image_args[@]}"

public_url_args=("${valid_args[@]}")
public_url_args[9]=http://0.0.0.0:8080/.well-known/agent-card.json
assert_rejected_before_mutation public-agent-card-url "${public_url_args[@]}"

wrong_context_args=("${valid_args[@]}")
wrong_context_args[1]=production-context
assert_rejected_before_mutation wrong-context "${wrong_context_args[@]}"

for boundary in \
	configmap-created \
	backup-job-created \
	agent-scaled \
	agent-deployment-scaled \
	mcp-scaled \
	restore-job-created; do
	case_dir="$(new_case "fail-${boundary}")"
	set_case_environment "${case_dir}"
	export FAKE_FAIL_AFTER="${boundary}"
	if run_drill "${case_dir}" "${case_dir}/stdout" "${case_dir}/stderr"; then
		echo "failure after ${boundary} unexpectedly passed" >&2
		exit 1
	fi
	unset FAKE_FAIL_AFTER
	assert_restored_and_owned_resources_removed "${case_dir}"
done

cleanup_failure_case="$(new_case cleanup-failure)"
set_case_environment "${cleanup_failure_case}"
export FAKE_FAIL_AFTER=mcp-scaled
export FAKE_CLEANUP_FAIL=backup-job
set +e
run_drill "${cleanup_failure_case}" "${cleanup_failure_case}/stdout" "${cleanup_failure_case}/stderr"
cleanup_failure_status=$?
set -e
unset FAKE_FAIL_AFTER FAKE_CLEANUP_FAIL
[[ ${cleanup_failure_status} == 91 ]] || {
	echo "cleanup masked primary status 91 with ${cleanup_failure_status}" >&2
	exit 1
}
grep -Fq 'Platform backup drill cleanup was incomplete' "${cleanup_failure_case}/stderr"
grep -Fq 'Preserving primary failure status 91' "${cleanup_failure_case}/stderr"
jq -e '.agent_cr == 3 and .agent_deployment == 2 and .mcp_deployment == 4' \
	"${cleanup_failure_case}/state/replicas.json" >/dev/null
[[ -f ${cleanup_failure_case}/state/resources/Job--unrelated-job.json ]]
[[ ! -e ${cleanup_failure_case}/docker-image-state ]]

success_case="$(new_case success)"
set_case_environment "${success_case}"
if ! run_drill "${success_case}" "${success_case}/stdout" "${success_case}/stderr"; then
	cat -- "${success_case}/stderr" >&2
	exit 1
fi
grep -Fxq 'platform backup restore drill verified' "${success_case}/stdout"
assert_restored_and_owned_resources_removed "${success_case}"
grep -Fq $'pull\t' "${success_case}/docker.log"
grep -Fq $'remove\t' "${success_case}/docker.log"

preexisting_case="$(new_case preexisting-image)"
set_case_environment "${preexisting_case}"
printf '%s\n' "${agent_image}" >"${preexisting_case}/docker-image-state"
if ! run_drill "${preexisting_case}" "${preexisting_case}/stdout" "${preexisting_case}/stderr"; then
	cat -- "${preexisting_case}/stderr" >&2
	exit 1
fi
assert_restored_and_owned_resources_removed "${preexisting_case}" present
[[ ! -s ${preexisting_case}/docker.log ]]

for invocation in \
	"${repo_dir}/.github/workflows/platform.yml" \
	"${repo_dir}/content/6. Platform/6.6. Platform Delivery.md"; do
	for flag in \
		--context \
		--work-dir \
		--source-revision \
		--reader-image \
		--agent-card-url \
		--docker-config \
		--evidence-marker; do
		grep -Fq -- "${flag}" "${invocation}"
	done
	grep -Fq 'http://127.0.0.1:8080/.well-known/agent-card.json' "${invocation}"
	grep -Eq 'curlimages/curl:[^[:space:]]+@sha256:[0-9a-f]{64}' "${invocation}"
done
grep -Fq -- '--field revision' "${repo_dir}/content/6. Platform/6.6. Platform Delivery.md"
if grep -Fq '.identity' "${repo_dir}/content/6. Platform/6.6. Platform Delivery.md"; then
	echo "platform delivery still reads the nonexistent source identity field" >&2
	exit 1
fi

echo "platform backup drill safety tests passed"
