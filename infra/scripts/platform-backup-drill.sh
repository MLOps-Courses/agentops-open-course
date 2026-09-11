#!/usr/bin/env bash

set -Eeuo pipefail

readonly namespace=agentops
readonly owner_label_key=agentops.fmind.dev/backup-drill-id

usage() {
	cat <<'EOF'
Usage: platform-backup-drill.sh \
  --context CONTEXT \
  --work-dir PRIVATE_DIRECTORY \
  --source-revision FULL_COMMIT \
  --reader-image IMAGE@sha256:DIGEST \
  --agent-card-url LOOPBACK_URL \
  --docker-config PRIVATE_DIRECTORY \
  --evidence-marker MARKER
EOF
}

fail() {
	printf 'platform backup drill: %s\n' "$*" >&2
	exit 1
}

require_value() {
	local flag="$1"
	local value="${2:-}"
	[[ -n ${value} ]] || fail "${flag} requires a value"
}

context=""
work_dir=""
source_revision=""
reader_image=""
agent_card_url=""
docker_config=""
evidence_marker=""

while (($# > 0)); do
	case "$1" in
	--context)
		require_value "$1" "${2:-}"
		[[ -z ${context} ]] || fail "--context may be supplied only once"
		context="$2"
		shift 2
		;;
	--work-dir)
		require_value "$1" "${2:-}"
		[[ -z ${work_dir} ]] || fail "--work-dir may be supplied only once"
		work_dir="$2"
		shift 2
		;;
	--source-revision)
		require_value "$1" "${2:-}"
		[[ -z ${source_revision} ]] || fail "--source-revision may be supplied only once"
		source_revision="$2"
		shift 2
		;;
	--reader-image)
		require_value "$1" "${2:-}"
		[[ -z ${reader_image} ]] || fail "--reader-image may be supplied only once"
		reader_image="$2"
		shift 2
		;;
	--agent-card-url)
		require_value "$1" "${2:-}"
		[[ -z ${agent_card_url} ]] || fail "--agent-card-url may be supplied only once"
		agent_card_url="$2"
		shift 2
		;;
	--docker-config)
		require_value "$1" "${2:-}"
		[[ -z ${docker_config} ]] || fail "--docker-config may be supplied only once"
		docker_config="$2"
		shift 2
		;;
	--evidence-marker)
		require_value "$1" "${2:-}"
		[[ -z ${evidence_marker} ]] || fail "--evidence-marker may be supplied only once"
		evidence_marker="$2"
		shift 2
		;;
	--help | -h)
		usage
		exit 0
		;;
	*)
		fail "unknown argument $1"
		;;
	esac
done

for required in context work_dir source_revision reader_image agent_card_url docker_config evidence_marker; do
	[[ -n ${!required} ]] || fail "--${required//_/-} is required; ambient environment values are not accepted"
done

for command_name in kubectl jq curl docker realpath stat sha256sum date cat dirname cut grep sleep; do
	command -v "${command_name}" >/dev/null 2>&1 || fail "missing required command ${command_name}"
done

[[ ${context} =~ ^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,252}$ ]] || fail "--context is malformed"
[[ ${source_revision} =~ ^[0-9a-f]{40}$ ]] || fail "--source-revision must be a full lowercase commit"
[[ ${reader_image} =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] ||
	fail "--reader-image must be pinned by sha256 digest"
[[ ${evidence_marker} =~ ^[a-z0-9][a-z0-9._-]{0,62}$ ]] ||
	fail "--evidence-marker must be a bounded lowercase identifier"

if [[ ${agent_card_url} =~ ^http://127\.0\.0\.1:([0-9]{1,5})(/[^[:space:]?#]*)?$ ]]; then
	agent_card_port="${BASH_REMATCH[1]}"
elif [[ ${agent_card_url} =~ ^http://\[::1\]:([0-9]{1,5})(/[^[:space:]?#]*)?$ ]]; then
	agent_card_port="${BASH_REMATCH[1]}"
else
	fail "--agent-card-url must use an explicit HTTP loopback address and port"
fi
((10#${agent_card_port} > 0 && 10#${agent_card_port} <= 65535)) ||
	fail "--agent-card-url port is outside 1-65535"

validate_private_directory() {
	local label="$1"
	local path="$2"
	local resolved
	local mode

	[[ ${path} == /* ]] || fail "${label} must be an absolute path"
	[[ -d ${path} && ! -L ${path} ]] || fail "${label} must be a real directory, not a symlink"
	resolved="$(realpath -e -- "${path}")" || fail "${label} cannot be resolved"
	[[ ${resolved} == "${path}" ]] || fail "${label} must be its canonical path with no symlink components"
	[[ -O ${path} ]] || fail "${label} must be owned by the current user"
	mode="$(stat -c '%a' -- "${path}")" || fail "${label} permissions cannot be read"
	[[ ${mode} =~ ^[0-7]{3,4}$ ]] || fail "${label} has an unrecognized permission mode"
	(((8#${mode} & 077) == 0)) || fail "${label} must not grant group or other access"
}

validate_private_directory "--work-dir" "${work_dir}"
validate_private_directory "--docker-config" "${docker_config}"
[[ ${docker_config} != "${work_dir}" ]] || fail "--docker-config must not be the artifact directory itself"

docker_config_file="${docker_config}/config.json"
readonly docker_config_file
[[ -f ${docker_config_file} && ! -L ${docker_config_file} && -O ${docker_config_file} ]] ||
	fail "--docker-config must contain a private regular config.json"
resolved_docker_config_file="$(realpath -e -- "${docker_config_file}")" ||
	fail "Docker config.json cannot be resolved"
readonly resolved_docker_config_file
[[ ${resolved_docker_config_file} == "${docker_config_file}" ]] ||
	fail "Docker config.json must be canonical and must not traverse a symlink"
docker_config_mode="$(stat -c '%a' -- "${docker_config_file}")"
readonly docker_config_mode
[[ ${docker_config_mode} =~ ^[0-7]{3,4}$ ]] || fail "Docker config.json has an unrecognized permission mode"
(((8#${docker_config_mode} & 077) == 0)) || fail "Docker config.json must not grant group or other access"
jq -e 'type == "object" and ((.auths // {}) | type == "object")' "${docker_config_file}" >/dev/null ||
	fail "Docker config.json is not a valid Docker authentication boundary"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
[[ -x ${script_dir}/assert-platform-build-info.sh ]] || fail "assert-platform-build-info.sh is unavailable"

for name in \
	AGENT_BUILD_MODE \
	AGENT_SOURCE_COMMIT \
	AGENT_SOURCE_REVISION \
	AGENT_SOURCE_TREE_DIGEST \
	AGENT_SOURCE_DIRTY \
	OCI_CREATED \
	OCI_VERSION; do
	[[ -n ${!name:-} ]] || fail "the validated build identity requires ${name}"
done
[[ ${AGENT_BUILD_MODE} == release && ${AGENT_SOURCE_DIRTY} == false ]] ||
	fail "the validated build identity must be a clean release"
[[ ${AGENT_SOURCE_COMMIT} == "${source_revision}" && ${AGENT_SOURCE_REVISION} == "${source_revision}" ]] ||
	fail "--source-revision disagrees with the validated build identity"
[[ ${AGENT_SOURCE_TREE_DIGEST} =~ ^sha256:[0-9a-f]{64}$ ]] || fail "the source tree digest is malformed"
[[ ${OCI_VERSION} =~ ^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
	fail "the candidate version is not stable semantic versioning"
jq -en --arg timestamp "${OCI_CREATED}" '$timestamp | fromdateiso8601' >/dev/null ||
	fail "the candidate build timestamp is not canonical UTC RFC3339"

readonly evidence_json="${work_dir}/platform-backup-evidence.json"
readonly manifest_json="${work_dir}/platform-backup-manifest.json"
readonly version_json="${work_dir}/platform-agent-version.json"
readonly card_json="${work_dir}/platform-agent-card.json"
readonly labels_json="${work_dir}/platform-agent-labels.json"
readonly backup_log="${work_dir}/platform-backup-job.log"
readonly restore_log="${work_dir}/platform-restore-job.log"
for output in "${evidence_json}" "${manifest_json}" "${version_json}" "${card_json}" "${labels_json}" "${backup_log}" "${restore_log}"; do
	[[ ! -e ${output} && ! -L ${output} ]] || fail "refusing to overwrite existing drill output ${output}"
done

kube() {
	kubectl --context "${context}" "$@"
}

run_docker() (
	unset DOCKER_AUTH_CONFIG
	export DOCKER_CONFIG="${docker_config}"
	exec docker "$@"
)

resolved_context="$(kube config get-contexts "${context}" --no-headers -o name)" ||
	fail "--context cannot be resolved"
readonly resolved_context
[[ ${resolved_context} == "${context}" ]] || fail "--context resolved to ${resolved_context:-nothing}, not ${context}"
kube --namespace "${namespace}" get namespace "${namespace}" -o name >/dev/null ||
	fail "the agentops namespace is unavailable through --context ${context}"
kube --namespace "${namespace}" get cronjob agentops-state-backup -o name >/dev/null ||
	fail "the state backup CronJob is unavailable"

capture_agent_replica_state() {
	jq -ce '
      (.spec.byo.deployment // {}) as $deployment |
      if ($deployment | has("replicas")) then
        ($deployment.replicas | select(type == "number" and . >= 0 and floor == .)) as $replicas |
        {present:true,value:$replicas}
      else
        {present:false,value:null}
      end
    '
}

agent_replica_state="$(kube --namespace "${namespace}" get agent agentops-agent -o json | capture_agent_replica_state)" ||
	fail "the Agent CR has no valid replica state"
agent_deployment_replicas="$(
	kube --namespace "${namespace}" get deployment agentops-agent -o json |
		jq -er '.spec.replicas | select(type == "number" and . >= 0 and floor == .)'
)" || fail "the agent Deployment has no valid replica count"
mcp_deployment_replicas="$(
	kube --namespace "${namespace}" get deployment agentops-mcp -o json |
		jq -er '.spec.replicas | select(type == "number" and . >= 0 and floor == .)'
)" || fail "the MCP Deployment has no valid replica count"
readonly agent_replica_state agent_deployment_replicas mcp_deployment_replicas

agent_pods_json="$(
	kube --namespace "${namespace}" get pods \
		-l app.kubernetes.io/name=agentops-agent \
		-o json
)" || fail "the agent pod inventory is unavailable"
readonly agent_pods_json
agent_pod="$(
	jq -er '
      [.items[] |
        select(.status.phase == "Running") |
        select(any(.status.containerStatuses[]?; .name == "agent" and .ready == true))] |
      if length == 1 then .[0].metadata.name
      else error("expected exactly one ready agent pod")
      end
    ' <<<"${agent_pods_json}"
)" || fail "the drill requires exactly one ready agent pod"
agent_image_id="$(
	jq -er --arg pod "${agent_pod}" '
      .items[] | select(.metadata.name == $pod) |
      .status.containerStatuses[] | select(.name == "agent") | .imageID
    ' <<<"${agent_pods_json}"
)" || fail "the running agent image identity is unavailable"
deployed_image_ref="${agent_image_id#*://}"
readonly agent_pod agent_image_id deployed_image_ref
[[ ${deployed_image_ref} =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] ||
	fail "the running agent does not expose a pullable digest identity"

kube --namespace "${namespace}" exec -i "${agent_pod}" --container agent -- \
	/app/agent version >"${version_json}"
curl --fail --silent --show-error "${agent_card_url}" >"${card_json}"
docker_image_preexisting=false
if run_docker image inspect "${deployed_image_ref}" >/dev/null 2>&1; then
	docker_image_preexisting=true
fi
readonly docker_image_preexisting

cleanup_docker_image() {
	[[ ${docker_image_preexisting} == false ]] || return 0
	if run_docker image inspect "${deployed_image_ref}" >/dev/null 2>&1; then
		run_docker image rm "${deployed_image_ref}" >/dev/null
	fi
}

begin_cleanup() {
	# A second signal must not strand resources after rollback has started.
	trap - EXIT
	trap '' INT TERM
}

cleanup_before_cluster_mutation() {
	local primary_status="$1"
	local cleanup_status=0

	begin_cleanup
	set +e
	cleanup_docker_image || {
		printf '::error::Could not remove the exact drill-pulled image digest.\n' >&2
		cleanup_status=1
	}
	if ((primary_status != 0)); then
		exit "${primary_status}"
	fi
	exit "${cleanup_status}"
}

# Pulling a missing image mutates the local Docker store, so its cleanup trap is
# active before the pull and is later subsumed by the cluster cleanup trap.
trap 'cleanup_before_cluster_mutation "$?"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ ${docker_image_preexisting} == false ]]; then
	run_docker pull "${deployed_image_ref}" >/dev/null
fi
run_docker image inspect --format '{{json .Config.Labels}}' "${deployed_image_ref}" >"${labels_json}"
jq -e --arg revision "${source_revision}" '.revision == $revision and .source_identity == $revision and .dirty == false' \
	"${version_json}" >/dev/null || fail "the running agent does not identify the requested clean revision"
jq -e --arg revision "${source_revision}" '
  .["org.opencontainers.image.revision"] == $revision and
  .["dev.fmind.agentops.source-revision"] == $revision and
  .["dev.fmind.agentops.source-dirty"] == "false"
' "${labels_json}" >/dev/null || fail "the pulled image labels do not identify the requested clean revision"
jq -e '.name == "AgentOps Agent" and (.version | type == "string" and length > 0)' "${card_json}" >/dev/null ||
	fail "the loopback AgentCard is malformed"

run_entropy="${source_revision}:${evidence_marker}:$$:${EPOCHREALTIME}"
run_id="$(printf '%s' "${run_entropy}" | sha256sum | cut -c1-12)"
backup_stamp="$(date -u +%Y%m%dT%H%M%SZ)"
readonly run_entropy run_id backup_stamp
[[ ${run_id} =~ ^[0-9a-f]{12}$ ]] || fail "could not construct a unique drill identity"
[[ ${backup_stamp} =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || fail "could not construct a backup timestamp"

readonly evidence_configmap="platform-backup-evidence-${run_id}"
readonly backup_job="platform-state-backup-${run_id}"
readonly restore_job="platform-state-restore-${run_id}"

for owned in "configmap:${evidence_configmap}" "job:${backup_job}" "job:${restore_job}"; do
	kind="${owned%%:*}"
	name="${owned#*:}"
	existing="$(kube --namespace "${namespace}" get "${kind}" "${name}" --ignore-not-found -o name)" ||
		fail "could not establish absence of ${kind} ${name}"
	[[ -z ${existing} ]] || fail "unique temporary ${kind} ${name} already exists"
done

agent_cr_mutation_attempted=false
agent_deployment_mutation_attempted=false
mcp_deployment_mutation_attempted=false
drill_completed=false

delete_owned_resource() {
	local kind="$1"
	local name="$2"
	local resource

	resource="$(kube --namespace "${namespace}" get "${kind}" "${name}" --ignore-not-found -o json)" || {
		printf '::error::Could not inspect temporary %s %s during cleanup.\n' "${kind}" "${name}" >&2
		return 1
	}
	[[ -n ${resource} ]] || return 0
	jq -e --arg key "${owner_label_key}" --arg owner "${run_id}" \
		'.metadata.labels[$key] == $owner' <<<"${resource}" >/dev/null || {
		printf '::error::Refusing to delete %s %s without this drill ownership label.\n' "${kind}" "${name}" >&2
		return 1
	}
	kube --namespace "${namespace}" delete "${kind}" "${name}" \
		--wait=true --timeout=60s >/dev/null
}

restore_agent_cr_replicas() {
	local patch
	patch="$(jq -cn --argjson state "${agent_replica_state}" '
      {spec:{byo:{deployment:{replicas:(if $state.present then $state.value else null end)}}}}
    ')"
	kube --namespace "${namespace}" patch agent agentops-agent --type=merge --patch "${patch}" >/dev/null
}

verify_restored_replicas() {
	local current_agent_state
	local current_agent_deployment
	local current_mcp_deployment

	current_agent_state="$(kube --namespace "${namespace}" get agent agentops-agent -o json | capture_agent_replica_state)" || return 1
	current_agent_deployment="$(kube --namespace "${namespace}" get deployment agentops-agent -o json | jq -er '.spec.replicas')" || return 1
	current_mcp_deployment="$(kube --namespace "${namespace}" get deployment agentops-mcp -o json | jq -er '.spec.replicas')" || return 1
	[[ ${current_agent_state} == "${agent_replica_state}" &&
		${current_agent_deployment} == "${agent_deployment_replicas}" &&
		${current_mcp_deployment} == "${mcp_deployment_replicas}" ]]
}

cleanup() {
	local primary_status="$1"
	local cleanup_failed=0
	local final_status="${primary_status}"

	begin_cleanup
	set +e
	delete_owned_resource job "${restore_job}" || cleanup_failed=1
	delete_owned_resource job "${backup_job}" || cleanup_failed=1

	if [[ ${agent_cr_mutation_attempted} == true ]]; then
		restore_agent_cr_replicas || {
			printf '::error::Could not restore the original Agent CR replica state.\n' >&2
			cleanup_failed=1
		}
	fi
	if [[ ${agent_deployment_mutation_attempted} == true ]]; then
		kube --namespace "${namespace}" scale deployment agentops-agent \
			--replicas="${agent_deployment_replicas}" >/dev/null || {
			printf '::error::Could not restore the original agent Deployment replicas.\n' >&2
			cleanup_failed=1
		}
	fi
	if [[ ${mcp_deployment_mutation_attempted} == true ]]; then
		kube --namespace "${namespace}" scale deployment agentops-mcp \
			--replicas="${mcp_deployment_replicas}" >/dev/null || {
			printf '::error::Could not restore the original MCP Deployment replicas.\n' >&2
			cleanup_failed=1
		}
	fi

	if [[ ${agent_deployment_mutation_attempted} == true && ${agent_deployment_replicas} -gt 0 ]]; then
		kube --namespace "${namespace}" rollout status deployment/agentops-agent --timeout=300s >/dev/null || {
			printf '::error::The restored agent Deployment did not become ready.\n' >&2
			cleanup_failed=1
		}
	fi
	if [[ ${mcp_deployment_mutation_attempted} == true && ${mcp_deployment_replicas} -gt 0 ]]; then
		kube --namespace "${namespace}" rollout status deployment/agentops-mcp --timeout=300s >/dev/null || {
			printf '::error::The restored MCP Deployment did not become ready.\n' >&2
			cleanup_failed=1
		}
	fi
	if [[ ${agent_cr_mutation_attempted} == true || ${agent_deployment_mutation_attempted} == true ||
		${mcp_deployment_mutation_attempted} == true ]]; then
		verify_restored_replicas || {
			printf '::error::Replica state differs from the exact pre-drill snapshot after cleanup.\n' >&2
			cleanup_failed=1
		}
	fi
	delete_owned_resource configmap "${evidence_configmap}" || cleanup_failed=1
	cleanup_docker_image || {
		printf '::error::Could not remove the exact drill-pulled image digest.\n' >&2
		cleanup_failed=1
	}

	if ((cleanup_failed != 0)); then
		printf '::error::Platform backup drill cleanup was incomplete.\n' >&2
		if ((primary_status == 0)); then
			final_status=1
		else
			printf '::error::Preserving primary failure status %d.\n' "${primary_status}" >&2
		fi
	fi
	if ((final_status == 0)) && [[ ${drill_completed} == true ]]; then
		printf 'platform backup restore drill verified\n'
	fi
	exit "${final_status}"
}

# From here onward the script can mutate persistent evidence, owned resources,
# or replica authorities. The trap is installed first and owns every rollback.
trap 'cleanup "$?"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

kube --namespace "${namespace}" exec -i "${agent_pod}" --container agent -- \
	/app/agent platform-backup seed \
	--marker "${evidence_marker}" \
	--state-dir /app/state \
	--data-dir /app/data \
	>"${evidence_json}"
jq -e --arg marker "${evidence_marker}" '
  .audit_invocation_id == $marker and
  (.session_id | type == "string" and length > 0) and
  (.task_id | type == "string" and length > 0) and
  (.memory_note | type == "string" and length > 0)
' "${evidence_json}" >/dev/null

kube --namespace "${namespace}" create configmap "${evidence_configmap}" \
	--from-file="evidence.json=${evidence_json}" \
	--dry-run=client -o json |
	jq --arg key "${owner_label_key}" --arg owner "${run_id}" \
		'.metadata.labels = ((.metadata.labels // {}) + {($key):$owner})' |
	kube create --filename - >/dev/null

kube --namespace "${namespace}" create job \
	--from=cronjob/agentops-state-backup \
	"${backup_job}" \
	--dry-run=client -o json |
	jq --arg stamp "${backup_stamp}" \
		--arg reader_image "${reader_image}" \
		--arg key "${owner_label_key}" \
		--arg owner "${run_id}" '
      .metadata.labels = ((.metadata.labels // {}) + {($key):$owner}) |
      .spec.template.metadata.labels = ((.spec.template.metadata.labels // {}) + {($key):$owner}) |
      .spec.template.spec.containers[0].env =
        ((.spec.template.spec.containers[0].env // []) +
          [{name:"STATE_BACKUP_TIMESTAMP",value:$stamp}]) |
      .spec.template.spec.containers += [{
        name:"manifest-reader",
        image:$reader_image,
        command:["sh","-eu","-c"],
        args:[
          "until test -f /backups/"+$stamp+"/.complete; do sleep 1; done; "+
          "cat /backups/"+$stamp+"/manifest.json"
        ],
        resources:{
          requests:{cpu:"10m",memory:"16Mi"},
          limits:{cpu:"50m",memory:"32Mi"}
        },
        securityContext:{
          allowPrivilegeEscalation:false,
          readOnlyRootFilesystem:true,
          capabilities:{drop:["ALL"]}
        },
        volumeMounts:[{name:"backups",mountPath:"/backups",readOnly:true}]
      }]
    ' |
	kube create --filename - >/dev/null

wait_for_job() {
	local job="$1"
	local timeout_seconds="$2"
	local deadline=$((SECONDS + timeout_seconds))
	local condition

	while ((SECONDS < deadline)); do
		condition="$(
			kube --namespace "${namespace}" get job "${job}" -o json |
				jq -r '
                  if any(.status.conditions[]?; .type == "Complete" and .status == "True") then "complete"
                  elif any(.status.conditions[]?; .type == "Failed" and .status == "True") then "failed"
                  else "pending"
                  end
                '
		)"
		case "${condition}" in
		complete) return 0 ;;
		pending) ;;
		failed)
			printf '::error::Job %s/%s failed.\n' "${namespace}" "${job}" >&2
			kube --namespace "${namespace}" logs "job/${job}" >&2 || true
			return 1
			;;
		*)
			printf '::error::Job %s/%s returned unknown condition %s.\n' "${namespace}" "${job}" "${condition}" >&2
			return 1
			;;
		esac
		sleep 2
	done
	printf '::error::Timed out waiting for Job %s/%s.\n' "${namespace}" "${job}" >&2
	kube --namespace "${namespace}" logs "job/${job}" >&2 || true
	return 1
}

wait_for_job "${backup_job}" 300
kube --namespace "${namespace}" logs "job/${backup_job}" --container backup >"${backup_log}"
kube --namespace "${namespace}" logs "job/${backup_job}" --container manifest-reader >"${manifest_json}"
jq -e '.source and (.databases | type == "array" and length > 0)' "${manifest_json}" >/dev/null
"${script_dir}/assert-platform-build-info.sh" \
	"${version_json}" \
	"${labels_json}" \
	"${card_json}" \
	"${manifest_json}" >/dev/null

agent_cr_mutation_attempted=true
kube --namespace "${namespace}" patch agent agentops-agent --type=merge \
	--patch '{"spec":{"byo":{"deployment":{"replicas":0}}}}' >/dev/null
agent_deployment_mutation_attempted=true
kube --namespace "${namespace}" scale deployment agentops-agent --replicas=0 >/dev/null
mcp_deployment_mutation_attempted=true
kube --namespace "${namespace}" scale deployment agentops-mcp --replicas=0 >/dev/null

writer_stop_deadline=$((SECONDS + 120))
readonly writer_stop_deadline
while :; do
	agent_pods="$(kube --namespace "${namespace}" get pods -l app.kubernetes.io/name=agentops-agent -o name)"
	mcp_pods="$(kube --namespace "${namespace}" get pods -l app.kubernetes.io/name=agentops-mcp -o name)"
	if [[ -z ${agent_pods} && -z ${mcp_pods} ]]; then
		break
	fi
	((SECONDS < writer_stop_deadline)) || fail "timed out while stopping state readers and writers"
	sleep 2
done

jq -n \
	--arg name "${restore_job}" \
	--arg owner_key "${owner_label_key}" \
	--arg owner "${run_id}" \
	--arg image "${deployed_image_ref}" \
	--arg snapshot "/backups/${backup_stamp}" \
	--arg revision "${source_revision}" \
	--arg evidence_configmap "${evidence_configmap}" '
  {
    apiVersion:"batch/v1",
    kind:"Job",
    metadata:{name:$name,namespace:"agentops",labels:{($owner_key):$owner}},
    spec:{
      backoffLimit:0,
      activeDeadlineSeconds:300,
      template:{
        metadata:{labels:{($owner_key):$owner}},
        spec:{
          serviceAccountName:"agentops-state-backup",
          automountServiceAccountToken:false,
          restartPolicy:"Never",
          securityContext:{
            runAsNonRoot:true,
            runAsUser:10001,
            runAsGroup:10001,
            fsGroup:10001,
            seccompProfile:{type:"RuntimeDefault"}
          },
          containers:[{
            name:"restore",
            image:$image,
            imagePullPolicy:"IfNotPresent",
            command:["/app/agent"],
            args:[
              "platform-backup",
              "restore-drill",
              ("--snapshot="+$snapshot),
              "--state-dir=/app/state",
              ("--expected-source-identity="+$revision),
              "--evidence=/evidence/evidence.json"
            ],
            resources:{
              requests:{cpu:"50m",memory:"128Mi"},
              limits:{cpu:"200m",memory:"256Mi"}
            },
            securityContext:{
              allowPrivilegeEscalation:false,
              readOnlyRootFilesystem:true,
              capabilities:{drop:["ALL"]}
            },
            volumeMounts:[
              {name:"backups",mountPath:"/backups",readOnly:true},
              {name:"evidence",mountPath:"/evidence",readOnly:true},
              {name:"state",mountPath:"/app/state"}
            ]
          }],
          volumes:[
            {name:"backups",persistentVolumeClaim:{claimName:"agentops-state-backups"}},
            {name:"evidence",configMap:{name:$evidence_configmap}},
            {name:"state",persistentVolumeClaim:{claimName:"agentops-agent-state"}}
          ]
        }
      }
    }
  }
' | kube create --filename - >/dev/null

wait_for_job "${restore_job}" 300
kube --namespace "${namespace}" logs "job/${restore_job}" --container restore >"${restore_log}"
grep -Fxq 'platform backup restore drill verified' "${restore_log}" ||
	fail "restore Job did not emit the exact completion evidence"
drill_completed=true
