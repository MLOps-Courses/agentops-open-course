#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd cmp uv

readonly auditor=agents/python/.venv/bin/pip-audit
if [[ ! -x ${auditor} ]]; then
	fail "${auditor}: missing auditor; run mise run install"
fi

# This spaCy model is an official, exact GitHub release wheel rather than a
# PyPI project, so PyPI has no advisory coordinate for pip-audit to query. Keep
# its reviewed source and lock hash fail-closed, then omit only that one data
# package from the advisory request. uv sync still verifies the wheel hash.
readonly reviewed_model_requirement="en-core-web-sm @ https://github.com/explosion/spacy-models/releases/download/en_core_web_sm-3.8.0/en_core_web_sm-3.8.0-py3-none-any.whl \\"
readonly reviewed_model_hash='    --hash=sha256:1932429db727d4bff3deed6b34cfc05df17794f4a52eeb26cf8928f7c1a0fb85'

audit_dir=$(mktemp -d "${TMPDIR:-/tmp}/agentops-vulnerabilities.XXXXXX")
readonly audit_dir
trap 'rm -rf -- "${audit_dir}"' EXIT
ambient_environment="${audit_dir}/ambient"
uv venv --python agents/python/.venv/bin/python "${ambient_environment}" >/dev/null
ambient_site="$(
	"${ambient_environment}/bin/python" -c \
		'import sysconfig; print(sysconfig.get_paths()["purelib"])'
)"
mkdir -p "${ambient_site}/agentops_ambient_contaminant-9.9.dist-info"
printf '%s\n' \
	'Metadata-Version: 2.1' \
	'Name: agentops-ambient-contaminant' \
	'Version: 9.9' \
	'License: Proprietary' \
	>"${ambient_site}/agentops_ambient_contaminant-9.9.dist-info/METADATA"
readonly ambient_site

audit_profile() {
	local label=$1
	local slug=$2
	local project=$3
	local dependency_profile=$4
	local requirements="${audit_dir}/${slug}.txt"
	local auditable_requirements="${audit_dir}/${slug}-auditable.txt"
	local ambient_requirements="${audit_dir}/${slug}-ambient.txt"
	local model_count
	local model_hash
	local model_requirement
	local requirements_digest
	local -a groups=()

	case "${dependency_profile}" in
	runtime) groups=(--no-default-groups) ;;
	development) ;;
	evaluation) groups=(--group eval) ;;
	*) fail "unknown dependency profile '${dependency_profile}'" ;;
	esac

	# Audit the lock export rather than an ambient interpreter. pip-audit 2.10
	# removed its --python option; a hash-pinned export is the smaller supported
	# boundary and cannot inherit optional packages from a contributor's .venv.
	uv export \
		--project "${project}" \
		--locked \
		--no-header \
		--no-emit-project \
		--quiet \
		--output-file "${requirements}" \
		"${groups[@]}"
	PYTHONPATH="${ambient_site}" \
		VIRTUAL_ENV="${ambient_environment}" \
		UV_PROJECT_ENVIRONMENT="${ambient_environment}" \
		uv export \
		--project "${project}" \
		--locked \
		--no-header \
		--no-emit-project \
		--quiet \
		--output-file "${ambient_requirements}" \
		"${groups[@]}"
	cmp -s "${requirements}" "${ambient_requirements}" ||
		fail "${label}: ambient Python state changed the lock export"
	printf '%s dependencies: clean and pre-populated lock-export verdicts are identical\n' "${label}"
	requirements_digest="$(sha256sum "${requirements}")"
	requirements_digest="${requirements_digest%% *}"

	# Remove exactly the reviewed non-PyPI model block. Any source, hash, count,
	# or requirement-format drift fails instead of silently widening this carve-out.
	if grep -q '^en-core-web-sm @ ' "${requirements}"; then
		model_count="$(grep -c '^en-core-web-sm @ ' "${requirements}")"
		[[ "${model_count}" -eq 1 ]] ||
			fail "${label}: expected exactly one en-core-web-sm requirement"
		model_requirement="$(grep -m 1 '^en-core-web-sm @ ' "${requirements}")"
		model_hash="$(
			awk '/^en-core-web-sm @ / { getline; print; exit }' "${requirements}"
		)"
		[[ "${model_requirement}" == "${reviewed_model_requirement}" ]] ||
			fail "${label}: unreviewed en-core-web-sm source"
		[[ "${model_hash}" == "${reviewed_model_hash}" ]] ||
			fail "${label}: unreviewed en-core-web-sm wheel hash"
		printf '%s dependencies: reviewed non-PyPI model source and hash\n' "${label}"
	fi
	awk '
		/^en-core-web-sm @ / { skip = 2; next }
		skip > 0 { skip -= 1; next }
		{ print }
	' "${requirements}" >"${auditable_requirements}"

	printf '%s dependencies: auditing %s lock export (%s)\n' \
		"${label}" \
		"${dependency_profile}" \
		"${requirements_digest}"
	"${auditor}" \
		--requirement "${auditable_requirements}" \
		--require-hashes \
		--disable-pip \
		--progress-spinner off \
		--strict
}

audit_profile "documentation" documentation . development
audit_profile "agent runtime" agent-runtime agents/python runtime
audit_profile "agent development" agent-development agents/python development
audit_profile "agent evaluation" agent-evaluation agents/python evaluation
audit_profile "MLflow runtime" mlflow-runtime infra/mlflow runtime
