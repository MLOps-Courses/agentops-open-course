#!/usr/bin/env bash

# Capture dynamic GKE disks before deletion and prove their exact cloud handles
# are absent afterward. Empty inventories are valid after partial deployments.

scripts_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${scripts_dir}/../.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${repo_dir}/scripts/lib.sh"

command_name=${1:-}
case "${command_name}" in
capture-pvs)
	require_cmd kubectl platform
	require_cmd jq base
	context=${2:?usage: gcp-lab-audit.sh capture-pvs <context> <audit-dir>}
	audit_dir=${3:?usage: gcp-lab-audit.sh capture-pvs <context> <audit-dir>}
	[[ -d ${audit_dir} && ! -L ${audit_dir} ]] ||
		fail "audit directory must be an existing real directory: ${audit_dir}"

	raw_tmp=$(mktemp "${audit_dir}/.pvs-before-delete.XXXXXX")
	tsv_tmp=$(mktemp "${audit_dir}/.pvs-before-delete.XXXXXX")
	cleanup() {
		rm -f -- "${raw_tmp}" "${tsv_tmp}"
	}
	trap cleanup EXIT

	kubectl --context "${context}" get pv -o json >"${raw_tmp}"
	jq -e '
      type == "object" and
      (.items | type == "array") and
      ([
        .items[] |
        select(
          .spec.claimRef.namespace == "agentops" or
          .spec.claimRef.namespace == "kagent"
        ) |
        select(
          (.metadata.name | type) != "string" or
          (.spec.csi.volumeHandle | type) != "string" or
          .spec.csi.volumeHandle == ""
        )
      ] | length == 0)
    ' "${raw_tmp}" >/dev/null
	jq -r '
      .items[] |
      select(
        .spec.claimRef.namespace == "agentops" or
        .spec.claimRef.namespace == "kagent"
      ) |
      [.metadata.name, .spec.claimRef.namespace, .spec.csi.volumeHandle] |
      @tsv
    ' "${raw_tmp}" | sort -u >"${tsv_tmp}"
	mv -- "${raw_tmp}" "${audit_dir}/pvs-before-delete.json"
	mv -- "${tsv_tmp}" "${audit_dir}/pvs-before-delete.tsv"
	trap - EXIT
	pv_count=$(wc -l <"${audit_dir}/pvs-before-delete.tsv")
	printf 'captured %s course PV handle(s)\n' "${pv_count}"
	;;
verify-disks)
	require_cmd gcloud gcp
	project_id=${2:?usage: gcp-lab-audit.sh verify-disks <project-id> <pv-inventory.tsv>}
	inventory_path=${3:?usage: gcp-lab-audit.sh verify-disks <project-id> <pv-inventory.tsv>}
	[[ -f ${inventory_path} && ! -L ${inventory_path} ]] ||
		fail "PV inventory must be a regular file: ${inventory_path}"

	while IFS=$'\t' read -r pv_name namespace volume_handle extra; do
		[[ -n ${pv_name} && -n ${namespace} && -n ${volume_handle} && -z ${extra} ]] ||
			fail "malformed PV inventory row in ${inventory_path}"
		disk_name=${volume_handle##*/}
		[[ ${disk_name} =~ ^[a-z][a-z0-9-]{0,62}$ ]] ||
			fail "unsafe disk name in PV inventory: ${disk_name}"
		remaining=$(
			gcloud compute disks list \
				--project "${project_id}" \
				--filter="name=${disk_name}" \
				--format='value(name)'
		)
		[[ -z ${remaining} ]] ||
			fail "CSI disk ${disk_name} still exists: ${remaining}"
	done <"${inventory_path}"
	printf 'captured CSI disks are absent\n'
	;;
*)
	fail "usage: $0 <capture-pvs|verify-disks> ..."
	;;
esac
