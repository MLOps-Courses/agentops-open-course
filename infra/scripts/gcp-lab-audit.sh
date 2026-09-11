#!/usr/bin/env bash

# jq filters deliberately keep jq's $variables inside single-quoted programs.
# shellcheck disable=SC2016

# Own the optional GCP lab from immutable approval through verified teardown.
# Every cloud query names the approved project explicitly; the phase ledger is
# written before mutation so an interrupted apply resumes as cleanup work.

scripts_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${scripts_dir}/../.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${repo_dir}/scripts/lib.sh"

# Files created by this lifecycle can contain project coordinates and raw
# inventories. Keep them private even when the caller has a permissive umask.
umask 077

readonly ledger_schema_version=1
readonly required_model_call_budget=6
readonly -a managed_services=(
	"aiplatform.googleapis.com"
	"artifactregistry.googleapis.com"
	"compute.googleapis.com"
	"container.googleapis.com"
	"iam.googleapis.com"
	"iamcredentials.googleapis.com"
	"serviceusage.googleapis.com"
	"storage.googleapis.com"
	"sts.googleapis.com"
)
readonly -a service_disable_order=(
	"aiplatform.googleapis.com"
	"container.googleapis.com"
	"artifactregistry.googleapis.com"
	"iamcredentials.googleapis.com"
	"sts.googleapis.com"
	"storage.googleapis.com"
	"compute.googleapis.com"
	"iam.googleapis.com"
	"serviceusage.googleapis.com"
)

usage() {
	cat >&2 <<'EOF'
usage:
  gcp-lab-audit.sh prepare <approval.json> <ledger-dir>
  gcp-lab-audit.sh preflight <ledger-dir> <apply-plan-sha256>
  gcp-lab-audit.sh apply <ledger-dir> <APPLY approval token>
  gcp-lab-audit.sh record <ledger-dir>
  gcp-lab-audit.sh destroy <ledger-dir> <DESTROY approval token>
  gcp-lab-audit.sh status <ledger-dir>
EOF
	exit 1
}

validate_approval() {
	local approval_path="$1"

	jq -e --argjson budget "${required_model_call_budget}" '
	  . as $approval |
	  type == "object" and
	  $approval.schema_version == 1 and
	  ($approval.project_id | type == "string" and test("^[a-z][a-z0-9-]{4,28}[a-z0-9]$")) and
	  ($approval.project_number | type == "string" and test("^[1-9][0-9]{5,19}$")) and
	  ($approval.operator_principal | type == "string" and test("^[^[:space:]@]+@[^[:space:]@]+$")) and
	  ($approval.adc_principal | type == "string" and test("^[^[:space:]@]+@[^[:space:]@]+$")) and
	  ($approval.adc_quota_project_id == $approval.project_id) and
	  ($approval.adc_quota_project_number == $approval.project_number) and
	  ($approval.adc_credentials_path | type == "string" and startswith("/")) and
	  ($approval.source_sha | type == "string" and test("^[0-9a-f]{40}$")) and
	  ($approval.region | type == "string" and test("^[a-z]+-[a-z]+[0-9]+$")) and
	  ($approval.zone | type == "string" and test("^[a-z]+-[a-z]+[0-9]+-[a-z]$")) and
	  ($approval.zone | startswith($approval.region + "-")) and
	  ($approval.cluster_name | type == "string" and test("^[a-z][a-z0-9-]{0,39}$")) and
	  ($approval.network_name | type == "string" and test("^[a-z][a-z0-9-]{0,62}$")) and
	  ($approval.subnetwork_name | type == "string" and test("^[a-z][a-z0-9-]{0,62}$")) and
	  ($approval.artifact_repository | type == "string" and test("^[a-z][a-z0-9-]{0,62}$")) and
	  ($approval.node_service_account == ("agentops-gke-nodes@" + $approval.project_id + ".iam.gserviceaccount.com")) and
	  ($approval.agentgateway_service_account == ("agentgateway@" + $approval.project_id + ".iam.gserviceaccount.com")) and
	  ($approval.bucket_names | type == "array" and all(.[];
	    type == "string" and test("^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$")
	  )) and
	  (($approval.bucket_names | length) == ($approval.bucket_names | unique | length)) and
	  ($approval.tofu_dir | type == "string" and startswith("/")) and
	  ($approval.state_path | type == "string" and startswith("/")) and
	  ($approval.kubeconfig | type == "string" and startswith("/")) and
	  ($approval.tfvars_path | type == "string" and startswith("/")) and
	  ($approval.tfvars_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
	  ($approval.control_plane_cidr | type == "string" and test("^([0-9]{1,3}\\.){3}[0-9]{1,3}/32$")) and
	  ($approval.max_spend_usd | type == "number" and . > 0) and
	  ($approval.estimated_spend_usd | type == "number" and . >= 0 and . <= $approval.max_spend_usd) and
	  ($approval.deadline_utc | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
	  ($approval.cleanup_reserve_minutes | type == "number" and . == floor and . >= 15) and
	  $approval.gcp_model_call_budget == $budget
	' "${approval_path}" >/dev/null || fail "approval contract is invalid"
}

validate_approved_paths() {
	local path_mode

	[[ -d ${tofu_dir} && ! -L ${tofu_dir} ]] ||
		fail "approved OpenTofu directory must be an existing real directory: ${tofu_dir}"
	[[ ${state_path} == "${tofu_dir}/terraform.tfstate" ]] ||
		fail "approved state path must be ${tofu_dir}/terraform.tfstate"
	[[ ${tfvars_path} == "${tofu_dir}/terraform.tfvars" ]] ||
		fail "approved variable path must be ${tofu_dir}/terraform.tfvars"
	for path in "${adc_credentials_path}" "${tfvars_path}"; do
		[[ -f ${path} && ! -L ${path} ]] ||
			fail "approved input must be a regular file: ${path}"
		path_mode="$(stat -c '%a' "${path}")"
		[[ ${path_mode} == 600 ]] || fail "approved input must have mode 600: ${path}"
	done
	[[ -d $(dirname -- "${kubeconfig}") && ! -L $(dirname -- "${kubeconfig}") ]] ||
		fail "approved kubeconfig parent must be an existing real directory: $(dirname -- "${kubeconfig}")"
	if [[ -e ${state_path} ]]; then
		[[ -f ${state_path} && ! -L ${state_path} ]] ||
			fail "approved OpenTofu state must be a regular file: ${state_path}"
		path_mode="$(stat -c '%a' "${state_path}")"
		[[ ${path_mode} == 600 ]] ||
			fail "approved OpenTofu state must have mode 600: ${state_path}"
	fi
	if [[ -e ${kubeconfig} ]]; then
		[[ -f ${kubeconfig} && ! -L ${kubeconfig} ]] ||
			fail "approved kubeconfig must be a regular file: ${kubeconfig}"
		path_mode="$(stat -c '%a' "${kubeconfig}")"
		[[ ${path_mode} == 600 ]] ||
			fail "approved kubeconfig must have mode 600: ${kubeconfig}"
	fi
}

load_ledger() {
	ledger_dir="$1"
	[[ -d ${ledger_dir} && ! -L ${ledger_dir} ]] ||
		fail "ledger directory must be an existing real directory: ${ledger_dir}"
	ledger_dir="$(cd -- "${ledger_dir}" && pwd -P)"
	ledger_path="${ledger_dir}/ledger.json"
	[[ -f ${ledger_path} && ! -L ${ledger_path} ]] ||
		fail "lifecycle ledger is missing or unsafe: ${ledger_path}"
	jq -e --argjson schema "${ledger_schema_version}" '
	  .schema_version == $schema and
	  (.phase | type == "string") and
	  (.history | type == "array") and
	  (.approval | type == "object")
	' "${ledger_path}" >/dev/null || fail "lifecycle ledger is malformed"
	approval_tmp="$(mktemp "${ledger_dir}/.approval.XXXXXX")"
	jq -S .approval "${ledger_path}" >"${approval_tmp}"
	validate_approval "${approval_tmp}"

	project_id="$(jq -er .project_id "${approval_tmp}")"
	project_number="$(jq -er .project_number "${approval_tmp}")"
	operator_principal="$(jq -er .operator_principal "${approval_tmp}")"
	adc_principal="$(jq -er .adc_principal "${approval_tmp}")"
	adc_quota_project_id="$(jq -er .adc_quota_project_id "${approval_tmp}")"
	adc_quota_project_number="$(jq -er .adc_quota_project_number "${approval_tmp}")"
	adc_credentials_path="$(jq -er .adc_credentials_path "${approval_tmp}")"
	source_sha="$(jq -er .source_sha "${approval_tmp}")"
	region="$(jq -er .region "${approval_tmp}")"
	zone="$(jq -er .zone "${approval_tmp}")"
	cluster_name="$(jq -er .cluster_name "${approval_tmp}")"
	network_name="$(jq -er .network_name "${approval_tmp}")"
	subnetwork_name="$(jq -er .subnetwork_name "${approval_tmp}")"
	artifact_repository="$(jq -er .artifact_repository "${approval_tmp}")"
	node_service_account="$(jq -er .node_service_account "${approval_tmp}")"
	agentgateway_service_account="$(jq -er .agentgateway_service_account "${approval_tmp}")"
	bucket_names="$(jq -c .bucket_names "${approval_tmp}")"
	tofu_dir="$(jq -er .tofu_dir "${approval_tmp}")"
	state_path="$(jq -er .state_path "${approval_tmp}")"
	kubeconfig="$(jq -er .kubeconfig "${approval_tmp}")"
	tfvars_path="$(jq -er .tfvars_path "${approval_tmp}")"
	tfvars_sha256="$(jq -er .tfvars_sha256 "${approval_tmp}")"
	control_plane_cidr="$(jq -er .control_plane_cidr "${approval_tmp}")"
	deadline_utc="$(jq -er .deadline_utc "${approval_tmp}")"
	cleanup_reserve_minutes="$(jq -er .cleanup_reserve_minutes "${approval_tmp}")"
	phase="$(jq -er .phase "${ledger_path}")"
	expected_context="gke_${project_id}_${zone}_${cluster_name}"
	repository_uri="${region}-docker.pkg.dev/${project_id}/${artifact_repository}"
	repository_resource="projects/${project_id}/locations/${region}/repositories/${artifact_repository}"
	apply_plan="${ledger_dir}/apply.tfplan"
	destroy_plan="${ledger_dir}/destroy.tfplan"

	case "${phase}" in
	preparing | prepared | preflighted | apply_started | applied | recorded | workloads_deleting | workloads_deleted | destroy_planning | destroy_planned | destroy_applying | infrastructure_destroyed | services_restoring | services_restored | verifying | complete) ;;
	*) fail "lifecycle ledger has unknown phase: ${phase}" ;;
	esac
	validate_approved_paths
	rm -f -- "${approval_tmp}"
}

update_ledger() {
	local filter="$1"
	shift
	local ledger_tmp

	ledger_tmp="$(mktemp "${ledger_dir}/.ledger.XXXXXX")"
	if ! jq "$@" "${filter}" "${ledger_path}" >"${ledger_tmp}"; then
		rm -f -- "${ledger_tmp}"
		fail "could not update lifecycle ledger"
	fi
	chmod 600 "${ledger_tmp}"
	mv -- "${ledger_tmp}" "${ledger_path}"
}

set_phase() {
	local next_phase="$1"
	local recorded_at

	recorded_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	update_ledger \
		'.phase = $phase | .history += [{phase: $phase, recorded_at: $recorded_at}]' \
		--arg phase "${next_phase}" \
		--arg recorded_at "${recorded_at}"
	phase="${next_phase}"
}

run_array_inventory() {
	local label="$1"
	shift
	local inventory

	if ! inventory="$("$@")"; then
		fail "${label} inventory failed"
	fi
	jq -e 'type == "array"' <<<"${inventory}" >/dev/null ||
		fail "${label} inventory was not a JSON array"
	printf '%s\n' "${inventory}"
}

run_object_inventory() {
	local label="$1"
	shift
	local inventory

	if ! inventory="$("$@")"; then
		fail "${label} inventory failed"
	fi
	jq -e 'type == "object"' <<<"${inventory}" >/dev/null ||
		fail "${label} inventory was not a JSON object"
	printf '%s\n' "${inventory}"
}

assert_inventory_empty() {
	local label="$1"
	local inventory="$2"
	local filter="$3"
	shift 3
	local remaining

	remaining="$(jq -c "$@" "${filter}" <<<"${inventory}")" ||
		fail "could not evaluate ${label} inventory"
	[[ ${remaining} == "[]" ]] || fail "${label} still exists: ${remaining}"
}

verify_source_agreement() {
	local actual_sha
	local source_status

	actual_sha="$(git -C "${repo_dir}" rev-parse HEAD 2>/dev/null)" ||
		fail "approved source revision is unavailable"
	[[ ${actual_sha} == "${source_sha}" ]] ||
		fail "source revision disagrees with the immutable approval"
	source_status="$(git -C "${repo_dir}" status --porcelain=v1 --untracked-files=all 2>/dev/null)" ||
		fail "approved source status is unavailable"
	[[ -z ${source_status} ]] || fail "GCP lifecycle requires a clean exact source revision"
}

verify_deadline_reserve() {
	local deadline_epoch
	local now_epoch
	local cleanup_epoch

	deadline_epoch="$(date -u -d "${deadline_utc}" +%s 2>/dev/null)" ||
		fail "approved GCP deadline is invalid"
	now_epoch="$(date -u +%s)"
	cleanup_epoch="$((now_epoch + cleanup_reserve_minutes * 60))"
	((cleanup_epoch < deadline_epoch)) ||
		fail "approved GCP deadline no longer leaves the cleanup reserve"
}

decode_jwt_payload() {
	local token="$1"
	local payload

	[[ ${token} == *.*.* ]] || return 1
	payload="${token#*.}"
	payload="${payload%%.*}"
	case $((${#payload} % 4)) in
	2) payload+="==" ;;
	3) payload+="=" ;;
	0) ;;
	*) return 1 ;;
	esac
	printf '%s' "${payload}" | tr -- '_-' '/+' | base64 -d 2>/dev/null
}

read_tfvar() {
	local expression="$1"
	local value

	if ! value="$(printf '%s\n' "${expression}" | tofu -chdir="${tofu_dir}" \
		console -var-file="${tfvars_path}")"; then
		fail "approved variable file could not resolve ${expression}"
	fi
	jq -r 'if type == "string" or type == "number" or type == "boolean" then . else error("not scalar") end' \
		<<<"${value}" || fail "approved variable ${expression} is not a scalar JSON value"
}

assert_tfvar_value() {
	local expression="$1"
	local expected="$2"
	local error_message="$3"
	local actual

	actual="$(read_tfvar "${expression}")"
	[[ ${actual} == "${expected}" ]] || fail "${error_message}"
}

verify_tfvars_agreement() {
	local authorized_networks

	verify_sha256 "${tfvars_path}" "${tfvars_sha256}" "approved terraform.tfvars"
	assert_tfvar_value 'var.project_id' "${project_id}" \
		"terraform.tfvars project disagrees with the immutable approval"
	assert_tfvar_value 'var.region' "${region}" \
		"terraform.tfvars region disagrees with the immutable approval"
	assert_tfvar_value 'var.zone' "${zone}" \
		"terraform.tfvars zone disagrees with the immutable approval"
	assert_tfvar_value 'var.cluster_name' "${cluster_name}" \
		"terraform.tfvars cluster disagrees with the immutable approval"
	assert_tfvar_value 'var.machine_type' "e2-standard-2" \
		"terraform.tfvars must use the tested e2-standard-2 machine type"
	assert_tfvar_value 'var.spot_nodes' "true" \
		"terraform.tfvars must keep Spot nodes enabled"
	assert_tfvar_value 'var.node_disk_size_gb' "30" \
		"terraform.tfvars must use the tested 30 GB node disk"
	assert_tfvar_value 'var.deletion_protection' "false" \
		"terraform.tfvars must disable deletion protection for the disposable lab"
	authorized_networks="$(printf '%s\n' 'jsonencode(var.master_authorized_networks)' |
		tofu -chdir="${tofu_dir}" console -var-file="${tfvars_path}")" ||
		fail "approved control-plane allowlist could not be resolved"
	authorized_networks="$(jq -er '.' <<<"${authorized_networks}")" ||
		fail "approved control-plane allowlist is malformed"
	jq -e --arg cidr "${control_plane_cidr}" '
	  type == "array" and length == 1 and
	  .[0].cidr_block == $cidr and
	  (.[0].display_name | type == "string" and length > 0)
	' <<<"${authorized_networks}" >/dev/null ||
		fail "terraform.tfvars control-plane allowlist disagrees with the approved /32"
}

verify_project_agreement() {
	local project_json
	local quota_project_json
	local active_principal
	local effective_project
	local adc_token
	local adc_payload
	local actual_adc_principal
	local actual_quota_project

	project_json="$(run_object_inventory "GCP project" \
		gcloud projects describe "${project_id}" --project "${project_id}" --format=json)"
	jq -e \
		--arg project_id "${project_id}" \
		--arg project_number "${project_number}" '
		  .projectId == $project_id and (.projectNumber | tostring) == $project_number
		' <<<"${project_json}" >/dev/null ||
		fail "active GCP project ID or number disagrees with the immutable approval"
	if ! active_principal="$(gcloud auth list --filter=status:ACTIVE --format='value(account)')"; then
		fail "active gcloud principal inventory failed"
	fi
	[[ ${active_principal} == "${operator_principal}" ]] ||
		fail "active gcloud principal disagrees with the immutable approval"
	if ! effective_project="$(gcloud config get-value project --quiet)"; then
		fail "effective gcloud project inventory failed"
	fi
	[[ -z ${effective_project} || ${effective_project} == "(unset)" || ${effective_project} == "${project_id}" ]] ||
		fail "effective gcloud project disagrees with the immutable approval"
	actual_quota_project="$(jq -er '.quota_project_id' "${adc_credentials_path}")" ||
		fail "approved ADC file has no quota project"
	[[ ${actual_quota_project} == "${adc_quota_project_id}" ]] ||
		fail "ADC quota project disagrees with the immutable approval"
	quota_project_json="$(run_object_inventory "ADC quota project" \
		gcloud projects describe "${actual_quota_project}" --project "${project_id}" --format=json)"
	jq -e --arg project_id "${adc_quota_project_id}" \
		--arg project_number "${adc_quota_project_number}" '
		  .projectId == $project_id and (.projectNumber | tostring) == $project_number
		' <<<"${quota_project_json}" >/dev/null ||
		fail "ADC quota project ID or number disagrees with the immutable approval"
	if ! adc_token="$(GOOGLE_APPLICATION_CREDENTIALS="${adc_credentials_path}" \
		gcloud auth application-default print-identity-token)"; then
		fail "Application Default Credentials are unusable"
	fi
	adc_payload="$(decode_jwt_payload "${adc_token}")" ||
		fail "Application Default Credentials returned an unreadable identity token"
	actual_adc_principal="$(jq -er '.email // .sub' <<<"${adc_payload}")" ||
		fail "Application Default Credentials identity is unavailable"
	[[ ${actual_adc_principal} == "${adc_principal}" ]] ||
		fail "ADC principal disagrees with the immutable approval"
}

state_resources() {
	if [[ ! -e ${state_path} ]]; then
		return 0
	fi
	[[ -f ${state_path} && ! -L ${state_path} ]] ||
		fail "approved OpenTofu state is not a regular file: ${state_path}"
	tofu -chdir="${tofu_dir}" state list -state="${state_path}"
}

require_empty_state() {
	local resources

	if ! resources="$(state_resources)"; then
		fail "OpenTofu state inventory failed"
	fi
	[[ -z ${resources} ]] || fail "approved OpenTofu state is not empty: ${resources}"
}

capture_baseline() {
	local services_json
	local iam_json
	local services_tmp
	local iam_tmp

	services_json="$(run_array_inventory "enabled services" \
		gcloud services list --enabled --project "${project_id}" --format=json)"
	services_tmp="$(mktemp "${ledger_dir}/.services-before.XXXXXX")"
	jq -er '.[].config.name' <<<"${services_json}" | sort -u >"${services_tmp}"
	mv -- "${services_tmp}" "${ledger_dir}/services-before.txt"

	iam_json="$(run_object_inventory "project IAM" \
		gcloud projects get-iam-policy "${project_id}" --format=json)"
	iam_tmp="$(mktemp "${ledger_dir}/.iam-before.XXXXXX")"
	jq -S . <<<"${iam_json}" >"${iam_tmp}"
	mv -- "${iam_tmp}" "${ledger_dir}/project-iam-before.json"
}

captured_disk_names() {
	if [[ ! -s ${ledger_dir}/pvs-before-delete.tsv ]]; then
		printf '[]\n'
		return 0
	fi
	jq -Rn '
	  [inputs
	    | split("\t")
	    | select(length == 3)
	    | .[2]
	    | split("/")
	    | last
	  ] | unique
	' <"${ledger_dir}/pvs-before-delete.tsv"
}

verify_owned_absence() {
	local inventory
	local captured_disks

	verify_project_agreement

	inventory="$(run_array_inventory "GKE clusters" \
		gcloud container clusters list --project "${project_id}" --format=json)"
	assert_inventory_empty "GKE cluster" "${inventory}" \
		'[.[] | select(.name == $name and .location == $zone)]' \
		--arg name "${cluster_name}" --arg zone "${zone}"

	inventory="$(run_array_inventory "Artifact Registry repositories" \
		gcloud artifacts repositories list --project "${project_id}" \
		--location "${region}" --format=json)"
	assert_inventory_empty "Artifact Registry repository" "${inventory}" \
		'[.[] | select(.name == $name)]' --arg name "${repository_resource}"

	inventory="$(run_array_inventory "VPC networks" \
		gcloud compute networks list --project "${project_id}" --format=json)"
	assert_inventory_empty "VPC network" "${inventory}" \
		'[.[] | select(.name == $name)]' --arg name "${network_name}"

	inventory="$(run_array_inventory "VPC subnetworks" \
		gcloud compute networks subnets list --project "${project_id}" \
		--regions "${region}" --format=json)"
	assert_inventory_empty "VPC subnetwork" "${inventory}" \
		'[.[] | select(.name == $name and (.region | endswith("/regions/" + $region)))]' \
		--arg name "${subnetwork_name}" --arg region "${region}"

	inventory="$(run_array_inventory "GKE instances" \
		gcloud compute instances list --project "${project_id}" --format=json)"
	assert_inventory_empty "GKE instances" "${inventory}" \
		'[.[] | select(.name | startswith("gke-" + $cluster + "-"))]' \
		--arg cluster "${cluster_name}"

	inventory="$(run_array_inventory "GKE managed instance groups" \
		gcloud compute instance-groups managed list --project "${project_id}" --format=json)"
	assert_inventory_empty "GKE managed instance groups" "${inventory}" \
		'[.[] | select(.name | startswith("gke-" + $cluster + "-"))]' \
		--arg cluster "${cluster_name}"

	inventory="$(run_array_inventory "GKE instance templates" \
		gcloud compute instance-templates list --project "${project_id}" --format=json)"
	assert_inventory_empty "GKE instance templates" "${inventory}" \
		'[.[] | select(.name | startswith("gke-" + $cluster + "-"))]' \
		--arg cluster "${cluster_name}"

	inventory="$(run_array_inventory "VPC firewall rules" \
		gcloud compute firewall-rules list --project "${project_id}" --format=json)"
	assert_inventory_empty "VPC firewall rules" "${inventory}" \
		'[.[] | select(.network | endswith("/networks/" + $network))]' \
		--arg network "${network_name}"

	inventory="$(run_array_inventory "VPC routes" \
		gcloud compute routes list --project "${project_id}" --format=json)"
	assert_inventory_empty "VPC routes" "${inventory}" \
		'[.[] | select(.network | endswith("/networks/" + $network))]' \
		--arg network "${network_name}"

	captured_disks="$(captured_disk_names)"
	inventory="$(run_array_inventory "persistent disks" \
		gcloud compute disks list --project "${project_id}" --format=json)"
	assert_inventory_empty "GKE or captured persistent disks" "${inventory}" '
	  [.[] | select(
	    .labels["goog-k8s-cluster-name"] == $cluster or
	    (.name as $name | $captured | index($name))
	  )]
	' --arg cluster "${cluster_name}" --argjson captured "${captured_disks}"

	inventory="$(run_array_inventory "Cloud Storage buckets" \
		gcloud storage buckets list --project "${project_id}" --format=json)"
	assert_inventory_empty "approved Cloud Storage buckets" "${inventory}" \
		'[.[] | select(.name as $name | $buckets | index($name))]' \
		--argjson buckets "${bucket_names}"

	inventory="$(run_array_inventory "service accounts" \
		gcloud iam service-accounts list --project "${project_id}" --format=json)"
	assert_inventory_empty "course service accounts" "${inventory}" '
	  [.[] | select(.email == $nodes or .email == $gateway)]
	' --arg nodes "${node_service_account}" --arg gateway "${agentgateway_service_account}"

	inventory="$(run_object_inventory "project IAM" \
		gcloud projects get-iam-policy "${project_id}" --format=json)"
	assert_inventory_empty "course project IAM members" "${inventory}" '
	  [.bindings[]?.members[]?
	    | select(. == ("serviceAccount:" + $nodes) or . == ("serviceAccount:" + $gateway))
	  ]
	' --arg nodes "${node_service_account}" --arg gateway "${agentgateway_service_account}"
}

verify_services_and_iam_baseline() {
	local services_json
	local services_now
	local iam_json
	local iam_now

	[[ -f ${ledger_dir}/services-before.txt && ! -L ${ledger_dir}/services-before.txt ]] ||
		fail "enabled-services baseline is missing"
	[[ -f ${ledger_dir}/project-iam-before.json && ! -L ${ledger_dir}/project-iam-before.json ]] ||
		fail "project-IAM baseline is missing"
	services_json="$(run_array_inventory "enabled services" \
		gcloud services list --enabled --project "${project_id}" --format=json)"
	services_now="$(mktemp "${ledger_dir}/.services-now.XXXXXX")"
	jq -er '.[].config.name' <<<"${services_json}" | sort -u >"${services_now}"
	if ! cmp -s "${ledger_dir}/services-before.txt" "${services_now}"; then
		rm -f -- "${services_now}"
		fail "enabled services do not match the pre-apply baseline"
	fi
	rm -f -- "${services_now}"

	iam_json="$(run_object_inventory "project IAM" \
		gcloud projects get-iam-policy "${project_id}" --format=json)"
	iam_now="$(mktemp "${ledger_dir}/.iam-now.XXXXXX")"
	jq -S . <<<"${iam_json}" >"${iam_now}"
	if ! cmp -s "${ledger_dir}/project-iam-before.json" "${iam_now}"; then
		rm -f -- "${iam_now}"
		fail "project IAM does not match the pre-apply baseline"
	fi
	rm -f -- "${iam_now}"
}

verify_exact_context() {
	[[ -f ${kubeconfig} && ! -L ${kubeconfig} ]] ||
		fail "approved isolated kubeconfig is missing: ${kubeconfig}"
	local current_context

	if ! current_context="$(kubectl --kubeconfig "${kubeconfig}" config current-context)"; then
		fail "approved kubeconfig context inventory failed"
	fi
	[[ ${current_context} == "${expected_context}" ]] ||
		fail "kubectl context is ${current_context}; expected ${expected_context}"
}

cluster_exists() {
	local inventory
	local count

	inventory="$(run_array_inventory "GKE clusters" \
		gcloud container clusters list --project "${project_id}" --format=json)"
	count="$(jq -er \
		--arg name "${cluster_name}" --arg zone "${zone}" \
		'[.[] | select(.name == $name and .location == $zone)] | length' \
		<<<"${inventory}")"
	[[ ${count} == 0 || ${count} == 1 ]] ||
		fail "GKE cluster inventory returned duplicate exact matches"
	[[ ${count} == 1 ]]
}

capture_pvs() {
	local raw_tmp
	local tsv_tmp
	local merged_tmp

	raw_tmp="$(mktemp "${ledger_dir}/.pvs-current.XXXXXX")"
	tsv_tmp="$(mktemp "${ledger_dir}/.pvs-current.XXXXXX")"
	merged_tmp="$(mktemp "${ledger_dir}/.pvs-merged.XXXXXX")"
	if ! kubectl --kubeconfig "${kubeconfig}" --context "${expected_context}" \
		get pv -o json >"${raw_tmp}"; then
		rm -f -- "${raw_tmp}" "${tsv_tmp}" "${merged_tmp}"
		fail "Kubernetes PV inventory failed"
	fi
	jq -e '
	  type == "object" and (.items | type == "array") and
	  all(.items[] | select(
	    .spec.claimRef.namespace == "agentops" or
	    .spec.claimRef.namespace == "kagent"
	  );
	    (.metadata.name | type == "string" and length > 0) and
	    (.spec.csi.volumeHandle | type == "string" and length > 0)
	  )
	' "${raw_tmp}" >/dev/null || {
		rm -f -- "${raw_tmp}" "${tsv_tmp}" "${merged_tmp}"
		fail "Kubernetes PV inventory was malformed"
	}
	jq -r '
	  .items[] |
	  select(
	    .spec.claimRef.namespace == "agentops" or
	    .spec.claimRef.namespace == "kagent"
	  ) |
	  [.metadata.name, .spec.claimRef.namespace, .spec.csi.volumeHandle] |
	  @tsv
	' "${raw_tmp}" | sort -u >"${tsv_tmp}"
	if [[ -f ${ledger_dir}/pvs-before-delete.tsv ]]; then
		cat "${ledger_dir}/pvs-before-delete.tsv" "${tsv_tmp}" | sort -u >"${merged_tmp}"
	else
		cp "${tsv_tmp}" "${merged_tmp}"
	fi
	mv -- "${raw_tmp}" "${ledger_dir}/pvs-last.json"
	mv -- "${merged_tmp}" "${ledger_dir}/pvs-before-delete.tsv"
	rm -f -- "${tsv_tmp}"
}

namespace_inventory() {
	run_object_inventory "Kubernetes namespace list" \
		kubectl --kubeconfig "${kubeconfig}" --context "${expected_context}" \
		get namespaces -o json
}

wait_for_captured_pvs_to_disappear() {
	local current
	local captured
	local remaining

	captured="$(jq -Rn '[inputs | split("\t") | .[0]] | unique' \
		<"${ledger_dir}/pvs-before-delete.tsv")"
	for _ in {1..60}; do
		if ! current="$(kubectl --kubeconfig "${kubeconfig}" --context "${expected_context}" \
			get pv -o json)"; then
			fail "Kubernetes PV absence inventory failed"
		fi
		remaining="$(jq -c --argjson captured "${captured}" '
		  [.items[]? | select(.metadata.name as $name | $captured | index($name))]
		' <<<"${current}")"
		[[ ${remaining} != "[]" ]] || return 0
		sleep 5
	done
	fail "captured Kubernetes PVs still exist after workload deletion: ${remaining}"
}

cleanup_workloads() {
	local namespaces

	if ! cluster_exists; then
		set_phase workloads_deleted
		return 0
	fi
	verify_exact_context
	capture_pvs
	set_phase workloads_deleting
	namespaces="$(namespace_inventory)"
	if jq -e '.items[]? | select(.metadata.name == "agentops")' <<<"${namespaces}" >/dev/null; then
		(
			cd "${repo_dir}/infra" || exit
			KUBECONFIG="${kubeconfig}" SKAFFOLD_DEFAULT_REPO="${repository_uri}" \
				skaffold delete \
				--filename skaffold.yaml \
				--profile gke \
				--kube-context "${expected_context}"
		)
		kubectl --kubeconfig "${kubeconfig}" --context "${expected_context}" \
			wait --for=delete namespace/agentops --timeout=300s
	fi
	namespaces="$(namespace_inventory)"
	if jq -e '.items[]? | select(.metadata.name == "kagent")' <<<"${namespaces}" >/dev/null; then
		KUBECONFIG="${kubeconfig}" helmfile \
			--file "${repo_dir}/infra/helmfile.yaml" \
			--kube-context "${expected_context}" \
			destroy
		kubectl --kubeconfig "${kubeconfig}" --context "${expected_context}" \
			delete namespace kagent --wait=true --timeout=300s
	fi
	wait_for_captured_pvs_to_disappear
	set_phase workloads_deleted
}

restore_managed_services() {
	local services_json
	local services_now
	local services_new
	local service
	local known=false
	local unapproved_service=""

	set_phase services_restoring
	services_json="$(run_array_inventory "enabled services" \
		gcloud services list --enabled --project "${project_id}" --format=json)"
	services_now="$(mktemp "${ledger_dir}/.services-now.XXXXXX")"
	services_new="$(mktemp "${ledger_dir}/.services-new.XXXXXX")"
	jq -er '.[].config.name' <<<"${services_json}" | sort -u >"${services_now}"
	comm -13 "${ledger_dir}/services-before.txt" "${services_now}" >"${services_new}"
	while IFS= read -r service; do
		[[ -n ${service} ]] || continue
		known=false
		for managed_service in "${managed_services[@]}"; do
			if [[ ${service} == "${managed_service}" ]]; then
				known=true
				break
			fi
		done
		if [[ ${known} != true ]]; then
			unapproved_service="${service}"
			break
		fi
	done <"${services_new}"
	if [[ -n ${unapproved_service} ]]; then
		rm -f -- "${services_now}" "${services_new}"
		fail "unapproved service appeared after apply; refusing to disable it: ${unapproved_service}"
	fi
	for service in "${service_disable_order[@]}"; do
		if rg -Fxq -- "${service}" "${services_new}"; then
			gcloud services disable "${service}" --project "${project_id}" --quiet
		fi
	done
	rm -f -- "${services_now}" "${services_new}"
	verify_services_and_iam_baseline
	set_phase services_restored
}

remove_local_residue() {
	local path

	for path in "${apply_plan}" "${destroy_plan}" "${kubeconfig}"; do
		if [[ -e ${path} ]]; then
			[[ -f ${path} && ! -L ${path} ]] ||
				fail "refusing to remove non-regular lifecycle residue: ${path}"
			rm -f -- "${path}"
		fi
	done
	for path in "${state_path}.lock.info" "${tofu_dir}/.terraform.tfstate.lock.info"; do
		[[ ! -e ${path} ]] || fail "OpenTofu lock residue still exists: ${path}"
	done
	[[ ! -e ${apply_plan} && ! -e ${destroy_plan} && ! -e ${kubeconfig} ]] ||
		fail "approved local lifecycle residue remains"
}

verify_and_complete() {
	set_phase verifying
	require_empty_state
	verify_owned_absence
	verify_services_and_iam_baseline
	remove_local_residue
	set_phase complete
	printf 'GCP lab teardown verified complete; ledger retained at %s\n' "${ledger_path}"
}

validate_apply_plan_json() {
	local plan_json="$1"

	jq -e \
		--arg project_id "${project_id}" \
		--arg region "${region}" \
		--arg zone "${zone}" \
		--arg cluster "${cluster_name}" \
		--arg cidr "${control_plane_cidr}" '
		  .variables.project_id.value == $project_id and
		  .variables.region.value == $region and
		  .variables.zone.value == $zone and
		  .variables.cluster_name.value == $cluster and
		  .variables.machine_type.value == "e2-standard-2" and
		  .variables.spot_nodes.value == true and
		  .variables.node_disk_size_gb.value == 30 and
		  .variables.deletion_protection.value == false and
		  (.variables.master_authorized_networks.value | type == "array" and length == 1) and
		  .variables.master_authorized_networks.value[0].cidr_block == $cidr and
		  (.resource_changes | type == "array" and length > 0) and
		  all(.resource_changes[]; .change.actions == ["create"])
		' <<<"${plan_json}" >/dev/null ||
		fail "saved apply plan disagrees with the immutable approval or disposable cost profile"
}

verify_apply_plan_digest() {
	local expected_digest
	local plan_mode

	[[ -f ${apply_plan} && ! -L ${apply_plan} ]] ||
		fail "reviewed apply plan is missing or unsafe: ${apply_plan}"
	plan_mode="$(stat -c '%a' "${apply_plan}")"
	[[ ${plan_mode} == 600 ]] ||
		fail "reviewed apply plan must have mode 600: ${apply_plan}"
	expected_digest="$(jq -er '.apply_plan.sha256' "${ledger_path}")" ||
		fail "reviewed apply-plan digest is absent from the lifecycle ledger"
	verify_sha256 "${apply_plan}" "${expected_digest}" "reviewed apply plan"
}

prepare_lifecycle() {
	local approval_path="$1"
	local requested_ledger_dir="$2"
	local ledger_tmp
	local approval_canonical
	local ledger_mode

	require_host_cmd gcloud "install the Google Cloud SDK from a reviewed host package source"
	require_cmd jq base
	require_cmd tofu gcp
	[[ -f ${approval_path} && ! -L ${approval_path} ]] ||
		fail "approval must be a regular file: ${approval_path}"
	[[ -d ${requested_ledger_dir} && ! -L ${requested_ledger_dir} ]] ||
		fail "ledger directory must be an existing real directory: ${requested_ledger_dir}"
	requested_ledger_dir="$(cd -- "${requested_ledger_dir}" && pwd -P)"
	ledger_mode="$(stat -c '%a' "${requested_ledger_dir}")"
	[[ ${ledger_mode} == 700 ]] ||
		fail "ledger directory must have mode 700: ${requested_ledger_dir}"
	approval_canonical="$(mktemp "${requested_ledger_dir}/.approval.XXXXXX")"
	jq -S . "${approval_path}" >"${approval_canonical}"
	validate_approval "${approval_canonical}"

	if [[ ! -e ${requested_ledger_dir}/ledger.json ]]; then
		ledger_tmp="$(mktemp "${requested_ledger_dir}/.ledger.XXXXXX")"
		jq -n --slurpfile approval "${approval_canonical}" \
			'{
			  schema_version: 1,
			  phase: "preparing",
			  approval: $approval[0],
			  history: [{phase: "preparing"}]
			}' >"${ledger_tmp}"
		chmod 600 "${ledger_tmp}"
		mv -- "${ledger_tmp}" "${requested_ledger_dir}/ledger.json"
	fi
	load_ledger "${requested_ledger_dir}"
	jq -e --slurpfile approval "${approval_canonical}" \
		'.approval == $approval[0]' "${ledger_path}" >/dev/null ||
		fail "existing ledger does not match the immutable approval"
	rm -f -- "${approval_canonical}"
	if [[ ${phase} != preparing ]]; then
		printf 'GCP lifecycle already prepared at phase %s\n' "${phase}"
		return 0
	fi
	[[ -f ${kubeconfig} && ! -L ${kubeconfig} ]] ||
		fail "prepare requires an existing isolated kubeconfig: ${kubeconfig}"
	verify_source_agreement
	verify_deadline_reserve
	verify_tfvars_agreement
	verify_project_agreement
	require_empty_state
	capture_baseline
	verify_owned_absence
	set_phase prepared
	printf 'GCP lifecycle prepared: %s\n' "${ledger_path}"
}

preflight_lifecycle() {
	local approved_digest="$1"
	local actual_digest
	local plan_json
	local recorded_at
	local plan_mode

	[[ ${approved_digest} =~ ^[0-9a-f]{64}$ ]] ||
		fail "approved apply-plan digest must be a lowercase SHA-256"
	case "${phase}" in
	preflighted)
		verify_apply_plan_digest
		printf 'GCP lifecycle preflight remains valid for %s\n' "${apply_plan}"
		return 0
		;;
	prepared) ;;
	*) fail "plan preflight requires phase prepared; current phase is ${phase}" ;;
	esac
	require_host_cmd gcloud "install the Google Cloud SDK from a reviewed host package source"
	require_cmd jq base
	require_cmd tofu gcp
	verify_source_agreement
	verify_deadline_reserve
	verify_tfvars_agreement
	verify_project_agreement
	require_empty_state
	[[ -f ${apply_plan} && ! -L ${apply_plan} ]] ||
		fail "reviewed apply plan is missing or unsafe: ${apply_plan}"
	plan_mode="$(stat -c '%a' "${apply_plan}")"
	[[ ${plan_mode} == 600 ]] ||
		fail "reviewed apply plan must have mode 600: ${apply_plan}"
	actual_digest="$(sha256_file "${apply_plan}")"
	[[ ${actual_digest} == "${approved_digest}" ]] ||
		fail "saved apply-plan digest disagrees with owner approval"
	if ! plan_json="$(tofu -chdir="${tofu_dir}" show -json "${apply_plan}")"; then
		fail "saved apply plan inventory failed"
	fi
	validate_apply_plan_json "${plan_json}"
	recorded_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	update_ledger '
	  .phase = "preflighted" |
	  .apply_plan = {
	    path: $path,
	    sha256: $sha256,
	    resource_addresses: ($plan.resource_changes | map(.address) | sort)
	  } |
	  .history += [{phase: "preflighted", recorded_at: $recorded_at}]
	' --arg path "${apply_plan}" --arg sha256 "${actual_digest}" \
		--argjson plan "${plan_json}" --arg recorded_at "${recorded_at}"
	phase=preflighted
	printf 'GCP apply plan preflighted at SHA-256 %s\n' "${actual_digest}"
}

apply_lifecycle() {
	local approval_token="$1"
	local expected_token
	local plan_json
	local plan_digest
	local approved_plan_digest
	local recorded_at

	approved_plan_digest="$(jq -er '.apply_plan.sha256' "${ledger_path}")"
	expected_token="APPLY:${project_id}:${project_number}:${zone}:${cluster_name}:${approved_plan_digest}"
	[[ ${approval_token} == "${expected_token}" ]] ||
		fail "apply approval token must equal ${expected_token}"
	case "${phase}" in
	preflighted) ;;
	apply_started) fail "apply was interrupted; resume with the destroy command" ;;
	*) fail "apply requires phase preflighted; current phase is ${phase}" ;;
	esac
	require_host_cmd gcloud "install the Google Cloud SDK from a reviewed host package source"
	require_cmd jq base
	require_cmd tofu gcp
	verify_source_agreement
	verify_deadline_reserve
	verify_tfvars_agreement
	verify_project_agreement
	require_empty_state
	verify_apply_plan_digest
	if ! plan_json="$(tofu -chdir="${tofu_dir}" show -json "${apply_plan}")"; then
		fail "saved apply plan inventory failed"
	fi
	validate_apply_plan_json "${plan_json}"
	plan_digest="$(sha256_file "${apply_plan}")"
	[[ ${plan_digest} == "${approved_plan_digest}" ]] ||
		fail "saved apply plan changed after preflight"
	recorded_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	update_ledger '
	  .phase = "apply_started" |
	  .history += [{phase: "apply_started", recorded_at: $recorded_at}]
	' --arg recorded_at "${recorded_at}"
	phase=apply_started
	tofu -chdir="${tofu_dir}" apply -input=false -auto-approve "${apply_plan}"
	set_phase applied
	printf 'GCP apply completed; record live state before smoke or destroy\n'
}

record_lifecycle() {
	local outputs
	local pv_count

	case "${phase}" in
	recorded)
		printf 'GCP live state is already recorded\n'
		return 0
		;;
	applied) ;;
	apply_started) fail "apply was interrupted; resume with the destroy command" ;;
	*) fail "record requires phase applied; current phase is ${phase}" ;;
	esac
	require_host_cmd gcloud "install the Google Cloud SDK from a reviewed host package source"
	require_cmd jq base
	require_cmd kubectl gcp
	require_cmd tofu gcp
	verify_source_agreement
	verify_tfvars_agreement
	verify_project_agreement
	verify_apply_plan_digest
	if ! outputs="$(tofu -chdir="${tofu_dir}" output -json)"; then
		fail "OpenTofu output inventory failed"
	fi
	jq -e \
		--arg project_id "${project_id}" \
		--arg region "${region}" \
		--arg zone "${zone}" \
		--arg cluster "${cluster_name}" \
		--arg network "${network_name}" \
		--arg subnetwork "${subnetwork_name}" \
		--arg repository "${repository_uri}" \
		--arg gateway "${agentgateway_service_account}" \
		--arg nodes "${node_service_account}" '
		  .project_id.value == $project_id and
		  .region.value == $region and
		  .cluster_zone.value == $zone and
		  .cluster_name.value == $cluster and
		  .network_name.value == $network and
		  .subnetwork_name.value == $subnetwork and
		  .artifact_registry_repository.value == $repository and
		  .agentgateway_service_account.value == $gateway and
		  .node_service_account.value == $nodes
		' <<<"${outputs}" >/dev/null ||
		fail "OpenTofu outputs disagree with the immutable approval"
	cluster_exists || fail "approved GKE cluster is absent after apply"
	verify_exact_context
	capture_pvs
	set_phase recorded
	pv_count="$(wc -l <"${ledger_dir}/pvs-before-delete.tsv")"
	printf 'GCP live state and %s PV handle(s) recorded\n' \
		"${pv_count}"
}

destroy_lifecycle() {
	local approval_token="$1"
	local expected_token
	local plan_json
	local plan_digest
	local recorded_at

	expected_token="DESTROY:${project_id}:${project_number}:${zone}:${cluster_name}"
	[[ ${approval_token} == "${expected_token}" ]] ||
		fail "destroy approval token must equal ${expected_token}"
	[[ ${phase} != preparing ]] ||
		fail "preparation is incomplete; rerun prepare before cleanup"
	require_host_cmd gcloud "install the Google Cloud SDK from a reviewed host package source"
	require_cmd jq base
	require_cmd tofu gcp
	verify_project_agreement

	if [[ ${phase} == complete ]]; then
		require_empty_state
		verify_owned_absence
		verify_services_and_iam_baseline
		remove_local_residue
		printf 'GCP lab teardown remains verified complete; ledger retained at %s\n' "${ledger_path}"
		return 0
	fi
	if [[ ${phase} == prepared ]]; then
		verify_and_complete
		return 0
	fi
	verify_apply_plan_digest
	if [[ ${phase} == preflighted ]]; then
		verify_and_complete
		return 0
	fi

	require_cmd helmfile platform
	require_cmd kubectl gcp
	require_cmd skaffold platform
	case "${phase}" in
	apply_started | applied | recorded | workloads_deleting) cleanup_workloads ;;
	workloads_deleted | destroy_planning | destroy_planned | destroy_applying | infrastructure_destroyed | services_restoring | services_restored | verifying) ;;
	*) fail "destroy cannot resume from phase ${phase}" ;;
	esac

	case "${phase}" in
	workloads_deleted | destroy_planning | destroy_planned | destroy_applying)
		set_phase destroy_planning
		tofu -chdir="${tofu_dir}" plan -destroy -input=false -out="${destroy_plan}"
		if ! plan_json="$(tofu -chdir="${tofu_dir}" show -json "${destroy_plan}")"; then
			fail "saved destroy plan inventory failed"
		fi
		jq -e '
		  all(.resource_changes[]?;
		    .change.actions == ["delete"] or .change.actions == ["no-op"]
		  )
		' <<<"${plan_json}" >/dev/null ||
			fail "saved destroy plan contains a create or update action"
		plan_digest="$(sha256_file "${destroy_plan}")"
		recorded_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		update_ledger '
		  .phase = "destroy_planned" |
		  .destroy_plan = {path: $path, sha256: $sha256} |
		  .history += [{phase: "destroy_planned", recorded_at: $recorded_at}]
		' --arg path "${destroy_plan}" --arg sha256 "${plan_digest}" --arg recorded_at "${recorded_at}"
		phase=destroy_planned
		set_phase destroy_applying
		tofu -chdir="${tofu_dir}" apply -input=false -auto-approve "${destroy_plan}"
		set_phase infrastructure_destroyed
		;;
	*) fail "destroy cannot plan from phase ${phase}" ;;
	esac

	case "${phase}" in
	infrastructure_destroyed | services_restoring) restore_managed_services ;;
	services_restored | verifying) ;;
	*) fail "destroy lost its phase after OpenTofu cleanup: ${phase}" ;;
	esac
	verify_and_complete
}

command_name="${1:-}"
case "${command_name}" in
prepare)
	[[ $# == 3 ]] || usage
	prepare_lifecycle "$2" "$3"
	;;
preflight)
	[[ $# == 3 ]] || usage
	load_ledger "$2"
	preflight_lifecycle "$3"
	;;
apply)
	[[ $# == 3 ]] || usage
	load_ledger "$2"
	apply_lifecycle "$3"
	;;
record)
	[[ $# == 2 ]] || usage
	load_ledger "$2"
	record_lifecycle
	;;
destroy)
	[[ $# == 3 ]] || usage
	load_ledger "$2"
	destroy_lifecycle "$3"
	;;
status)
	[[ $# == 2 ]] || usage
	require_cmd jq base
	load_ledger "$2"
	jq '{schema_version, phase, approval, history, apply_plan, destroy_plan}' "${ledger_path}"
	;;
*) usage ;;
esac
