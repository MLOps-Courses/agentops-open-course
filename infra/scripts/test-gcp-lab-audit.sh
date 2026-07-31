#!/usr/bin/env bash

scripts_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${scripts_dir}/../.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${repo_dir}/scripts/lib.sh"

tmp_dir=$(mktemp -d)
trap 'rm -rf -- "${tmp_dir}"' EXIT
mkdir -p "${tmp_dir}/bin" "${tmp_dir}/audit"

cat >"${tmp_dir}/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
if [[ ${FAKE_KUBECTL_FAIL:-false} == true ]]; then
	exit 23
fi
printf '%s\n' "${FAKE_KUBECTL_JSON:?}"
EOF

cat >"${tmp_dir}/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
if [[ ${FAKE_GCLOUD_FAIL:-false} == true ]]; then
	exit 24
fi
for argument in "$@"; do
	if [[ ${argument} == --filter=name=* ]]; then
		disk_name=${argument#--filter=name=}
		printf '%s\n' "${disk_name}" >>"${FAKE_GCLOUD_LOG:?}"
		if [[ ${disk_name} == "${FAKE_REMAINING_DISK:-}" ]]; then
			printf '%s\n' "${disk_name}"
		fi
	fi
done
EOF
chmod +x "${tmp_dir}/bin/kubectl" "${tmp_dir}/bin/gcloud"

export PATH="${tmp_dir}/bin:${PATH}"
export FAKE_GCLOUD_LOG="${tmp_dir}/gcloud.log"

export FAKE_KUBECTL_JSON='{"items":[]}'
"${scripts_dir}/gcp-lab-audit.sh" capture-pvs test-context "${tmp_dir}/audit"
[[ ! -s ${tmp_dir}/audit/pvs-before-delete.tsv ]]

export FAKE_KUBECTL_JSON='{
  "items": [
    {
      "metadata": {"name": "pv-agent"},
      "spec": {
        "claimRef": {"namespace": "agentops"},
        "csi": {"volumeHandle": "projects/p/zones/z/disks/pvc-agent"}
      }
    },
    {
      "metadata": {"name": "pv-kagent"},
      "spec": {
        "claimRef": {"namespace": "kagent"},
        "csi": {"volumeHandle": "projects/p/zones/z/disks/pvc-kagent"}
      }
    },
    {
      "metadata": {"name": "pv-unrelated"},
      "spec": {
        "claimRef": {"namespace": "other"},
        "csi": {"volumeHandle": "projects/p/zones/z/disks/pvc-other"}
      }
    }
  ]
}'
"${scripts_dir}/gcp-lab-audit.sh" capture-pvs test-context "${tmp_dir}/audit"
expected=$'pv-agent\tagentops\tprojects/p/zones/z/disks/pvc-agent\npv-kagent\tkagent\tprojects/p/zones/z/disks/pvc-kagent'
[[ $(<"${tmp_dir}/audit/pvs-before-delete.tsv") == "${expected}" ]]

export FAKE_KUBECTL_JSON='{"items":"malformed"}'
if "${scripts_dir}/gcp-lab-audit.sh" capture-pvs test-context "${tmp_dir}/audit" >/dev/null 2>&1; then
	fail "malformed Kubernetes inventory was accepted"
fi
FAKE_KUBECTL_FAIL=true
export FAKE_KUBECTL_FAIL
if "${scripts_dir}/gcp-lab-audit.sh" capture-pvs test-context "${tmp_dir}/audit" >/dev/null 2>&1; then
	fail "failed kubectl inventory was accepted"
fi
unset FAKE_KUBECTL_FAIL

: >"${tmp_dir}/empty.tsv"
"${scripts_dir}/gcp-lab-audit.sh" verify-disks test-project "${tmp_dir}/empty.tsv"
: >"${FAKE_GCLOUD_LOG}"
"${scripts_dir}/gcp-lab-audit.sh" verify-disks test-project "${tmp_dir}/audit/pvs-before-delete.tsv"
sorted_disks=$(sort "${FAKE_GCLOUD_LOG}")
[[ ${sorted_disks} == $'pvc-agent\npvc-kagent' ]]

FAKE_REMAINING_DISK=pvc-kagent
export FAKE_REMAINING_DISK
if "${scripts_dir}/gcp-lab-audit.sh" verify-disks \
	test-project "${tmp_dir}/audit/pvs-before-delete.tsv" >/dev/null 2>&1; then
	fail "remaining CSI disk was accepted"
fi

printf 'GCP PV capture and exact disk absence checks passed\n'
