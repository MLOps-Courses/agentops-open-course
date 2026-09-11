#!/usr/bin/env bash

scripts_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${scripts_dir}/../.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${repo_dir}/scripts/lib.sh"

tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${tmp_dir}"' EXIT
mkdir -p "${tmp_dir}/bin"

cat >"${tmp_dir}/bin/fake-gcp-lifecycle" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

tool="$(basename -- "$0")"
state_dir="${FAKE_GCP_STATE_DIR:?}"
printf '%s\t%s\n' "${tool}" "$*" >>"${state_dir}/commands.log"

json_array_from_lines() {
	local field="$1"
	local path="$2"
	jq -Rn --arg field "${field}" '
	  [inputs | select(length > 0) | {($field): .}]
	' <"${path}"
}

remove_line() {
	local value="$1"
	local path="$2"
	awk -v value="${value}" '$0 != value' "${path}" >"${path}.tmp"
	mv -- "${path}.tmp" "${path}"
}

case "${tool}" in
gcloud)
	if [[ -n ${FAKE_INVENTORY_FAILURE:-} && $* == *"${FAKE_INVENTORY_FAILURE}"* ]]; then
		exit 44
	fi
	case "$*" in
	"projects describe "*)
		jq -n \
			--arg project_id "$(<"${state_dir}/project-id")" \
			--arg project_number "$(<"${state_dir}/project-number")" \
			'{projectId: $project_id, projectNumber: $project_number}'
		;;
	"config get-value project "*)
		cat "${state_dir}/project-id"
		;;
	"auth application-default print-identity-token")
		cat "${state_dir}/adc-token"
		;;
	"auth list "*)
		cat "${state_dir}/principal"
		;;
	"services list "*)
		jq -Rn '[inputs | select(length > 0) | {config: {name: .}}]' \
			<"${state_dir}/services"
		;;
	"services disable "*)
		service="$3"
		remove_line "${service}" "${state_dir}/services"
		;;
	"iam service-accounts list "*)
		json_array_from_lines email "${state_dir}/service-accounts"
		;;
	"projects get-iam-policy "*)
		cat "${state_dir}/iam.json"
		;;
	"container clusters list "*)
		if [[ -s ${state_dir}/cluster ]]; then
			jq -n \
				--arg name "$(<"${state_dir}/cluster")" \
				--arg location "$(<"${state_dir}/zone")" \
				'[{name: $name, location: $location}]'
		else
			printf '[]\n'
		fi
		;;
	"artifacts repositories list "*)
		if [[ -s ${state_dir}/repository ]]; then
			jq -n --arg name "projects/$(<"${state_dir}/project-id")/locations/$(<"${state_dir}/region")/repositories/$(<"${state_dir}/repository")" '[{name: $name}]'
		else
			printf '[]\n'
		fi
		;;
	"compute networks list "*)
		json_array_from_lines name "${state_dir}/network"
		;;
	"compute networks subnets list "*)
		if [[ -s ${state_dir}/subnetwork ]]; then
			jq -n \
				--arg name "$(<"${state_dir}/subnetwork")" \
				--arg region "https://example.test/regions/$(<"${state_dir}/region")" \
				'[{name: $name, region: $region}]'
		else
			printf '[]\n'
		fi
		;;
	"compute instances list "*)
		json_array_from_lines name "${state_dir}/instances"
		;;
	"compute instance-groups managed list "*)
		json_array_from_lines name "${state_dir}/managed-instance-groups"
		;;
	"compute instance-templates list "*)
		json_array_from_lines name "${state_dir}/instance-templates"
		;;
	"compute firewall-rules list "*)
		if [[ -s ${state_dir}/firewall-rules ]]; then
			jq -Rn \
				--arg network "https://example.test/global/networks/$(<"${state_dir}/network")" \
				'[inputs | select(length > 0) | {name: ., network: $network}]' \
				<"${state_dir}/firewall-rules"
		else
			printf '[]\n'
		fi
		;;
	"compute routes list "*)
		if [[ -s ${state_dir}/routes ]]; then
			jq -Rn \
				--arg network "https://example.test/global/networks/$(<"${state_dir}/network")" \
				'[inputs | select(length > 0) | {name: ., network: $network}]' \
				<"${state_dir}/routes"
		else
			printf '[]\n'
		fi
		;;
	"compute disks list "*)
		jq -Rn \
			--arg cluster "$(<"${state_dir}/approved-cluster")" \
			'[inputs | select(length > 0) | {name: ., labels: {"goog-k8s-cluster-name": $cluster}}]' \
			<"${state_dir}/disks"
		;;
	"storage buckets list "*)
		json_array_from_lines name "${state_dir}/buckets"
		;;
	*)
		printf 'unexpected fake gcloud call: %s\n' "$*" >&2
		exit 64
		;;
	esac
	;;
tofu)
	case "$*" in
	*" state list"*)
		cat "${state_dir}/tofu-resources"
		;;
	*" show -json "*"apply.tfplan"*)
		cat "${state_dir}/apply-plan.json"
		;;
	*" show -json "*"destroy.tfplan"*)
		cat "${state_dir}/destroy-plan.json"
		;;
	*" output -json"*)
		cat "${state_dir}/outputs.json"
		;;
	*" console -var-file="*)
		IFS= read -r expression
		case "${expression}" in
		var.project_id) key=project_id ;;
		var.region) key=region ;;
		var.zone) key=zone ;;
		var.cluster_name) key=cluster_name ;;
		var.machine_type)
			printf '"e2-standard-2"\n'
			exit 0
			;;
		var.spot_nodes)
			printf 'true\n'
			exit 0
			;;
		var.node_disk_size_gb)
			printf '30\n'
			exit 0
			;;
		var.deletion_protection)
			printf 'false\n'
			exit 0
			;;
		'jsonencode(var.master_authorized_networks)')
			jq -Rn --arg value '[{"cidr_block":"203.0.113.10/32","display_name":"operator"}]' '$value'
			exit 0
			;;
		*) exit 64 ;;
		esac
		value="$(awk -F= -v key="${key}" '
		  $1 ~ "^[[:space:]]*" key "[[:space:]]*$" {
		    value=$2
		    gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
		    gsub(/^"|"$/, "", value)
		    print value
		  }
		' "${FAKE_TFVARS_PATH:?}")"
		jq -Rn --arg value "${value}" '$value'
		;;
	*" plan -destroy "*)
		for argument in "$@"; do
			if [[ ${argument} == -out=* ]]; then
				: >"${argument#-out=}"
				break
			fi
		done
		;;
	*" apply "*"apply.tfplan"*)
		printf 'google_container_cluster.agentops\n' >"${state_dir}/tofu-resources"
		printf 'agentops\n' >"${state_dir}/cluster"
		printf 'agentops\n' >"${state_dir}/network"
		if [[ ${FAKE_APPLY_MODE:-complete} == partial ]]; then
			exit 42
		fi
		printf 'agentops-europe-west1\n' >"${state_dir}/subnetwork"
		printf 'agentops\n' >"${state_dir}/repository"
		printf '%s\n' \
			'agentops-gke-nodes@test-project.iam.gserviceaccount.com' \
			'agentgateway@test-project.iam.gserviceaccount.com' \
			>"${state_dir}/service-accounts"
		printf '%s\n' \
			'aiplatform.googleapis.com' \
			'artifactregistry.googleapis.com' \
			'compute.googleapis.com' \
			'container.googleapis.com' \
			'iam.googleapis.com' \
			'iamcredentials.googleapis.com' \
			'serviceusage.googleapis.com' \
			'storage.googleapis.com' \
			'sts.googleapis.com' \
			| sort -u >"${state_dir}/services"
		cp "${state_dir}/iam-owned.json" "${state_dir}/iam.json"
		printf '%s\n' agentops kagent >"${state_dir}/namespaces"
		cp "${state_dir}/pvs-owned.json" "${state_dir}/pvs.json"
		: >"${FAKE_APPROVED_STATE_PATH:?}"
		;;
	*" apply "*"destroy.tfplan"*)
		: >"${state_dir}/cluster"
		: >"${state_dir}/network"
		: >"${state_dir}/subnetwork"
		: >"${state_dir}/repository"
		: >"${state_dir}/instances"
		: >"${state_dir}/managed-instance-groups"
		: >"${state_dir}/instance-templates"
		: >"${state_dir}/firewall-rules"
		: >"${state_dir}/routes"
		: >"${state_dir}/disks"
		if [[ ${FAKE_DESTROY_INTERRUPT_ONCE:-false} == true && ! -e ${state_dir}/destroy-interrupted ]]; then
			: >"${state_dir}/destroy-interrupted"
			exit 43
		fi
		: >"${state_dir}/service-accounts"
		: >"${state_dir}/tofu-resources"
		cp "${state_dir}/iam-baseline.json" "${state_dir}/iam.json"
		;;
	*)
		printf 'unexpected fake tofu call: %s\n' "$*" >&2
		exit 64
		;;
	esac
	;;
git)
	case "$*" in
	*" rev-parse HEAD") cat "${state_dir}/source-sha" ;;
	*" status --porcelain=v1 --untracked-files=all") ;;
	*) exit 64 ;;
	esac
	;;
kubectl)
	if [[ ${FAKE_KUBECTL_INVENTORY_FAILURE:-false} == true && $* == *" get pv "* ]]; then
		exit 45
	fi
	case "$*" in
	*" config current-context")
		cat "${state_dir}/context"
		;;
	*" get pv -o json")
		cat "${state_dir}/pvs.json"
		;;
	*" get namespaces -o json")
		jq -Rn '{items: [inputs | select(length > 0) | {metadata: {name: .}}]}' \
			<"${state_dir}/namespaces"
		;;
	*" wait --for=delete namespace/agentops "*)
		;;
	*" delete namespace kagent "*)
		remove_line kagent "${state_dir}/namespaces"
		;;
	*)
		printf 'unexpected fake kubectl call: %s\n' "$*" >&2
		exit 64
		;;
	esac
	;;
skaffold)
	remove_line agentops "${state_dir}/namespaces"
	printf '{"items":[]}\n' >"${state_dir}/pvs.json"
	;;
helmfile)
	remove_line kagent "${state_dir}/namespaces"
	;;
*)
	printf 'unexpected fake tool: %s\n' "${tool}" >&2
	exit 64
	;;
esac
EOF
chmod +x "${tmp_dir}/bin/fake-gcp-lifecycle"
for command_name in gcloud tofu git kubectl skaffold helmfile; do
	ln -s fake-gcp-lifecycle "${tmp_dir}/bin/${command_name}"
done

export PATH="${tmp_dir}/bin:${PATH}"

initialize_case() {
	local case_name="$1"
	case_dir="${tmp_dir}/${case_name}"
	local state_dir="${case_dir}/fake-state"

	mkdir -p "${case_dir}/tofu" "${case_dir}/ledger" "${state_dir}"
	chmod 700 "${case_dir}/tofu" "${case_dir}/ledger" "${state_dir}"
	: >"${case_dir}/kubeconfig"
	chmod 600 "${case_dir}/kubeconfig"
	for name in \
		buckets cluster commands.log disks firewall-rules instance-templates instances \
		managed-instance-groups namespaces network repository routes service-accounts \
		services subnetwork tofu-resources; do
		: >"${state_dir}/${name}"
	done
	printf '%s\n' test-project >"${state_dir}/project-id"
	printf '%s\n' 123456789012 >"${state_dir}/project-number"
	printf '%s\n' operator@example.test >"${state_dir}/principal"
	printf '%s\n' europe-west1 >"${state_dir}/region"
	printf '%s\n' europe-west1-b >"${state_dir}/zone"
	printf '%s\n' agentops >"${state_dir}/approved-cluster"
	printf 'a%.0s' {1..40} >"${state_dir}/source-sha"
	printf '\n' >>"${state_dir}/source-sha"
	printf '%s\n' gke_test-project_europe-west1-b_agentops >"${state_dir}/context"
	adc_header="$(printf '%s' '{"alg":"none","typ":"JWT"}' | base64 -w0 | tr '+/' '-_' | tr -d '=')"
	adc_payload="$(printf '%s' '{"email":"adc@example.test"}' | base64 -w0 | tr '+/' '-_' | tr -d '=')"
	printf '%s.%s.signature\n' "${adc_header}" "${adc_payload}" >"${state_dir}/adc-token"
	printf '%s\n' serviceusage.googleapis.com >"${state_dir}/services"
	printf '{"bindings":[]}\n' >"${state_dir}/iam-baseline.json"
	cp "${state_dir}/iam-baseline.json" "${state_dir}/iam.json"
	printf '%s\n' \
		'{"bindings":[{"role":"roles/aiplatform.user","members":["serviceAccount:agentgateway@test-project.iam.gserviceaccount.com"]}]}' \
		>"${state_dir}/iam-owned.json"
	printf '%s\n' \
		'{"items":[' \
		'{"metadata":{"name":"pv-agent"},"spec":{"claimRef":{"namespace":"agentops"},"csi":{"volumeHandle":"projects/test-project/zones/europe-west1-b/disks/pvc-agent"}}},' \
		'{"metadata":{"name":"pv-kagent"},"spec":{"claimRef":{"namespace":"kagent"},"csi":{"volumeHandle":"projects/test-project/zones/europe-west1-b/disks/pvc-kagent"}}}' \
		']}' | tr -d '\n' >"${state_dir}/pvs-owned.json"
	printf '\n' >>"${state_dir}/pvs-owned.json"
	printf '{"items":[]}\n' >"${state_dir}/pvs.json"
	jq -n \
		'{
		  variables: {
		    project_id: {value: "test-project"},
		    region: {value: "europe-west1"},
		    zone: {value: "europe-west1-b"},
		    cluster_name: {value: "agentops"},
		    machine_type: {value: "e2-standard-2"},
		    spot_nodes: {value: true},
		    node_disk_size_gb: {value: 30},
		    deletion_protection: {value: false},
		    master_authorized_networks: {value: [{cidr_block: "203.0.113.10/32", display_name: "operator"}]}
		  },
		  resource_changes: [{address: "google_container_cluster.agentops", change: {actions: ["create"]}}]
		}' >"${state_dir}/apply-plan.json"
	jq -n \
		'{resource_changes: [{change: {actions: ["delete"]}}]}' \
		>"${state_dir}/destroy-plan.json"
	jq -n \
		'{
		  project_id: {value: "test-project"},
		  cluster_name: {value: "agentops"},
		  cluster_zone: {value: "europe-west1-b"},
		  region: {value: "europe-west1"},
		  network_name: {value: "agentops"},
		  subnetwork_name: {value: "agentops-europe-west1"},
		  artifact_registry_repository: {value: "europe-west1-docker.pkg.dev/test-project/agentops"},
		  agentgateway_service_account: {value: "agentgateway@test-project.iam.gserviceaccount.com"},
		  node_service_account: {value: "agentops-gke-nodes@test-project.iam.gserviceaccount.com"}
		}' >"${state_dir}/outputs.json"
	printf '%s\n' \
		'project_id = "test-project"' \
		'region = "europe-west1"' \
		'zone = "europe-west1-b"' \
		'cluster_name = "agentops"' \
		>"${case_dir}/tofu/terraform.tfvars"
	printf '%s\n' \
		'{"type":"authorized_user","quota_project_id":"test-project"}' \
		>"${case_dir}/adc.json"
	chmod 600 "${case_dir}/tofu/terraform.tfvars" "${case_dir}/adc.json"
	local tfvars_sha256
	tfvars_sha256="$(sha256sum "${case_dir}/tofu/terraform.tfvars" | awk '{ print $1 }')"
	jq -n \
		--arg tofu_dir "${case_dir}/tofu" \
		--arg state_path "${case_dir}/tofu/terraform.tfstate" \
		--arg kubeconfig "${case_dir}/kubeconfig" \
		--arg adc_credentials_path "${case_dir}/adc.json" \
		--arg tfvars_path "${case_dir}/tofu/terraform.tfvars" \
		--arg tfvars_sha256 "${tfvars_sha256}" \
		'{
		  schema_version: 1,
		  project_id: "test-project",
		  project_number: "123456789012",
		  operator_principal: "operator@example.test",
		  adc_principal: "adc@example.test",
		  adc_quota_project_id: "test-project",
		  adc_quota_project_number: "123456789012",
		  adc_credentials_path: $adc_credentials_path,
		  source_sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		  region: "europe-west1",
		  zone: "europe-west1-b",
		  cluster_name: "agentops",
		  network_name: "agentops",
		  subnetwork_name: "agentops-europe-west1",
		  artifact_repository: "agentops",
		  node_service_account: "agentops-gke-nodes@test-project.iam.gserviceaccount.com",
		  agentgateway_service_account: "agentgateway@test-project.iam.gserviceaccount.com",
		  bucket_names: [],
		  tofu_dir: $tofu_dir,
		  state_path: $state_path,
		  kubeconfig: $kubeconfig,
		  tfvars_path: $tfvars_path,
		  tfvars_sha256: $tfvars_sha256,
		  control_plane_cidr: "203.0.113.10/32",
		  max_spend_usd: 25,
		  estimated_spend_usd: 5,
		  deadline_utc: "2099-01-01T00:00:00Z",
		  cleanup_reserve_minutes: 30,
		  gcp_model_call_budget: 6
		}' >"${case_dir}/approval.json"

	export FAKE_GCP_STATE_DIR="${state_dir}"
	export FAKE_TFVARS_PATH="${case_dir}/tofu/terraform.tfvars"
	export FAKE_APPROVED_STATE_PATH="${case_dir}/tofu/terraform.tfstate"
	unset FAKE_APPLY_MODE FAKE_DESTROY_INTERRUPT_ONCE FAKE_INVENTORY_FAILURE
}

phase() {
	jq -er .phase "$1/ledger/ledger.json"
}

assert_phase() {
	local case_path="$1"
	local expected="$2"
	local actual

	actual="$(phase "${case_path}")"
	[[ ${actual} == "${expected}" ]] ||
		fail "lifecycle phase is ${actual}; expected ${expected}"
}

approval_token() {
	local action="$1"
	local plan_digest

	if [[ ${action} == APPLY ]]; then
		plan_digest="$(jq -er '.apply_plan.sha256' "${case_dir}/ledger/ledger.json")"
		printf '%s:test-project:123456789012:europe-west1-b:agentops:%s\n' \
			"${action}" "${plan_digest}"
		return 0
	fi
	printf '%s:test-project:123456789012:europe-west1-b:agentops\n' "${action}"
}

run_lifecycle_action() {
	local action="$1"
	local case_path="$2"
	local token

	token="$(approval_token "${action^^}")"
	"${scripts_dir}/gcp-lab-audit.sh" "${action}" \
		"${case_path}/ledger" "${token}"
}

prepare_case() {
	local case_dir="$1"
	"${scripts_dir}/gcp-lab-audit.sh" prepare \
		"${case_dir}/approval.json" "${case_dir}/ledger"
}

write_apply_plan() {
	local case_dir="$1"
	: >"${case_dir}/ledger/apply.tfplan"
	chmod 600 "${case_dir}/ledger/apply.tfplan"
}

preflight_case() {
	local case_dir="$1"
	local plan_sha256

	plan_sha256="$(sha256sum "${case_dir}/ledger/apply.tfplan" | awk '{ print $1 }')"
	"${scripts_dir}/gcp-lab-audit.sh" preflight \
		"${case_dir}/ledger" "${plan_sha256}"
}

# Empty state is valid, reaches complete without an apply, and remains
# independently re-verifiable on a repeated cleanup invocation.
initialize_case empty
prepare_case "${case_dir}"
assert_phase "${case_dir}" prepared
run_lifecycle_action destroy "${case_dir}"
assert_phase "${case_dir}" complete
[[ -f ${case_dir}/ledger/ledger.json && ! -e ${case_dir}/kubeconfig ]]
run_lifecycle_action destroy "${case_dir}"
assert_phase "${case_dir}" complete
if rg -q $'^tofu\t.* apply ' "${FAKE_GCP_STATE_DIR}/commands.log"; then
	fail "empty cleanup invoked OpenTofu apply"
fi

# A complete lifecycle records exact live state, captures dynamic PV handles,
# destroys through the saved ledger, verifies absence, and retains evidence.
initialize_case complete
prepare_case "${case_dir}"
write_apply_plan "${case_dir}"
preflight_case "${case_dir}"
assert_phase "${case_dir}" preflighted
run_lifecycle_action apply "${case_dir}"
assert_phase "${case_dir}" applied
"${scripts_dir}/gcp-lab-audit.sh" record "${case_dir}/ledger"
assert_phase "${case_dir}" recorded
captured_pv_count="$(wc -l <"${case_dir}/ledger/pvs-before-delete.tsv")"
assert_eq 'captured PersistentVolume rows' "${captured_pv_count}" 2
run_lifecycle_action destroy "${case_dir}"
assert_phase "${case_dir}" complete
[[ ! -s ${FAKE_GCP_STATE_DIR}/cluster && ! -s ${FAKE_GCP_STATE_DIR}/disks ]]
[[ ! -e ${case_dir}/ledger/apply.tfplan && ! -e ${case_dir}/ledger/destroy.tfplan ]]

# A failed apply never becomes retryable source work: the ledger stays at
# apply_started and the destroy command resumes cleanup from partial state.
initialize_case partial
prepare_case "${case_dir}"
write_apply_plan "${case_dir}"
preflight_case "${case_dir}"
export FAKE_APPLY_MODE=partial
if run_lifecycle_action apply "${case_dir}" >/dev/null 2>&1; then
	fail "partial apply unexpectedly passed"
fi
assert_phase "${case_dir}" apply_started
unset FAKE_APPLY_MODE
run_lifecycle_action destroy "${case_dir}"
assert_phase "${case_dir}" complete

# Project agreement is checked against the immutable project ID and number
# before the lifecycle can leave its non-destructive preparing phase.
initialize_case wrong-project
printf '%s\n' other-project >"${FAKE_GCP_STATE_DIR}/project-id"
if prepare_case "${case_dir}" >/dev/null 2>&1; then
	fail "wrong GCP project was accepted"
fi
assert_phase "${case_dir}" preparing
if rg -q $'^(tofu\t.* apply |skaffold\t|helmfile\t)' \
	"${FAKE_GCP_STATE_DIR}/commands.log"; then
	fail "wrong-project preflight reached a destructive command"
fi

# Inventory failures retain the immutable ledger and resume preparation after
# the read boundary recovers; an empty result is never synthesized on error.
initialize_case inventory-failure
export FAKE_INVENTORY_FAILURE="compute networks list"
if prepare_case "${case_dir}" >/dev/null 2>&1; then
	fail "failed cloud inventory was accepted"
fi
assert_phase "${case_dir}" preparing
unset FAKE_INVENTORY_FAILURE
prepare_case "${case_dir}"
assert_phase "${case_dir}" prepared

# ADC and tfvars are part of the immutable preflight, not ambient inputs.
initialize_case wrong-adc
adc_header="$(printf '%s' '{"alg":"none","typ":"JWT"}' | base64 -w0 | tr '+/' '-_' | tr -d '=')"
adc_payload="$(printf '%s' '{"email":"other@example.test"}' | base64 -w0 | tr '+/' '-_' | tr -d '=')"
printf '%s.%s.signature\n' "${adc_header}" "${adc_payload}" >"${FAKE_GCP_STATE_DIR}/adc-token"
if prepare_case "${case_dir}" >/dev/null 2>&1; then
	fail "wrong ADC principal was accepted"
fi
assert_phase "${case_dir}" preparing

initialize_case changed-tfvars
printf 'project_id = "other-project"\n' >"${case_dir}/tofu/terraform.tfvars"
chmod 600 "${case_dir}/tofu/terraform.tfvars"
if prepare_case "${case_dir}" >/dev/null 2>&1; then
	fail "changed tfvars were accepted"
fi
assert_phase "${case_dir}" preparing

initialize_case wrong-plan-hash
prepare_case "${case_dir}"
write_apply_plan "${case_dir}"
zero_digest="$(printf '0%.0s' {1..64})"
if "${scripts_dir}/gcp-lab-audit.sh" preflight \
	"${case_dir}/ledger" "${zero_digest}" >/dev/null 2>&1; then
	fail "wrong saved-plan digest was accepted"
fi
assert_phase "${case_dir}" prepared
preflight_case "${case_dir}"
assert_phase "${case_dir}" preflighted

# An interrupted destroy remains resumable. The next invocation regenerates a
# destroy plan from current state, proves all absence, and only then completes.
initialize_case interrupted-destroy
prepare_case "${case_dir}"
write_apply_plan "${case_dir}"
preflight_case "${case_dir}"
run_lifecycle_action apply "${case_dir}"
"${scripts_dir}/gcp-lab-audit.sh" record "${case_dir}/ledger"
export FAKE_DESTROY_INTERRUPT_ONCE=true
if run_lifecycle_action destroy "${case_dir}" >/dev/null 2>&1; then
	fail "interrupted destroy unexpectedly passed"
fi
assert_phase "${case_dir}" destroy_applying
unset FAKE_DESTROY_INTERRUPT_ONCE
run_lifecycle_action destroy "${case_dir}"
assert_phase "${case_dir}" complete
# The resumed run must regenerate its own destroy plan rather than replay the
# interrupted one, so exactly two saved destroy plans are expected.
destroy_plan_count="$(rg -c $'^tofu\t.* plan -destroy ' "${FAKE_GCP_STATE_DIR}/commands.log")"
assert_eq 'saved destroy plans' "${destroy_plan_count}" 2

printf 'GCP lifecycle ledger and resumable cleanup checks passed\n'
