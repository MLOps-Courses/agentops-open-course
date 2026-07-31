#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd cmp base jq rg uv sha256sum

profile=${1:-full}
readonly profile
if (($# > 1)); then
	fail "usage: $0 [core|full]"
fi
case "${profile}" in
core | full) ;;
*) fail "unknown license profile '${profile}'; expected core or full" ;;
esac

readonly allowed_licenses_json='[
  "3-Clause BSD License",
  "Apache 2.0",
  "Apache License 2.0",
  "Apache Software License",
  "Apache Software License; BSD License",
  "Apache Software License; MIT License",
  "Apache-2.0",
  "Apache-2.0 AND BSD-2-Clause",
  "Apache-2.0 AND CNRI-Python",
  "Apache-2.0 AND MIT",
  "Apache-2.0 OR BSD-2-Clause",
  "Apache-2.0 OR BSD-3-Clause",
  "BSD License",
  "BSD-2-Clause",
  "BSD-3-Clause",
  "BSD-3-Clause AND 0BSD AND MIT AND Zlib AND CC0-1.0",
  "ISC License (ISCL)",
  "MIT",
  "MIT AND PSF-2.0",
  "MIT License",
  "MIT License, Apache License, Version 2.0",
  "MIT-0",
  "MIT-CMU",
  "MPL-2.0",
  "MPL-2.0 AND MIT",
  "MPL-2.0 and MIT and BSD-3-Clause",
  "Mozilla Public License 2.0 (MPL 2.0)",
  "PSF-2.0",
  "Python Software Foundation License"
]'
readonly pip_licenses=.venv/bin/pip-licenses
inventory_dir=$(mktemp -d "${TMPDIR:-/tmp}/agentops-licenses.XXXXXX")
readonly inventory_dir
trap 'rm -rf -- "${inventory_dir}"' EXIT

check_repository_licenses() {
	local software_license
	local font_license=docs/assets/fmind/OFL-1.1.txt

	test -f LICENSE
	test -f docs/LICENSE.txt
	for software_license in agents/LICENSE clients/LICENSE infra/LICENSE load/LICENSE; do
		if ! cmp -s LICENSE "${software_license}"; then
			printf '%s: software license differs from the root MIT license\n' "${software_license}" >&2
			return 1
		fi
	done
	test -f "${font_license}"
	rg -Fq "Copyright (c) 2016 The Inter Project Authors" "${font_license}"
	rg -Fq "Copyright 2021 The Outfit Project Authors" "${font_license}"
	rg -Fq "SIL OPEN FONT LICENSE Version 1.1" "${font_license}"

	printf 'repository licenses: MIT software + CC BY 4.0 course content + OFL 1.1 fonts\n'
}

write_inventory() {
	local environment=$1
	local output=$2

	"${pip_licenses}" \
		--python "${environment}/bin/python" \
		--from mixed \
		--with-license-file \
		--no-license-path \
		--format json \
		>"${output}"
	jq -S 'sort_by(.Name | ascii_downcase)' "${output}" >"${output}.canonical"
	mv "${output}.canonical" "${output}"
}

sync_inventory() {
	local label=$1
	local slug=$2
	local project=$3
	local dependency_profile=$4
	local output=$5
	local environment="${inventory_dir}/venvs/${slug}"
	local contaminated="${inventory_dir}/${slug}-contaminated.json"
	local resynchronized="${inventory_dir}/${slug}-resynchronized.json"
	local site_packages
	local -a groups=()
	local inventory_digest

	if [[ ! -x ${pip_licenses} ]]; then
		printf '%s: missing license checker; run mise run install:core\n' "${pip_licenses}" >&2
		return 1
	fi

	case "${dependency_profile}" in
	runtime) groups=(--no-default-groups) ;;
	development) ;;
	evaluation) groups=(--group eval) ;;
	*) fail "unknown dependency profile '${dependency_profile}'" ;;
	esac

	# UV_PROJECT_ENVIRONMENT makes the inventory independent of every ambient
	# project .venv. `uv sync` removes undeclared packages, so a clean checkout
	# and a previously populated workstation resolve the same lock-owned set.
	UV_PROJECT_ENVIRONMENT="${environment}" \
		uv sync \
		--project "${project}" \
		--locked \
		--no-install-project \
		--quiet \
		"${groups[@]}"

	write_inventory "${environment}" "${output}"

	# Prove the exact-sync claim rather than trusting uv's documented default.
	# A fake proprietary distribution must appear before resynchronization, then
	# disappear, leaving a byte-identical lock-owned inventory.
	site_packages="$("${environment}/bin/python" -c 'import sysconfig; print(sysconfig.get_paths()["purelib"])')"
	mkdir -p "${site_packages}/agentops_ambient_contaminant-9.9.dist-info"
	printf '%s\n' \
		'Metadata-Version: 2.1' \
		'Name: agentops-ambient-contaminant' \
		'Version: 9.9' \
		'License: Proprietary' \
		>"${site_packages}/agentops_ambient_contaminant-9.9.dist-info/METADATA"
	printf '%s\n' 'uv' >"${site_packages}/agentops_ambient_contaminant-9.9.dist-info/INSTALLER"
	printf '%s\n' \
		'agentops_ambient_contaminant-9.9.dist-info/METADATA,,' \
		'agentops_ambient_contaminant-9.9.dist-info/INSTALLER,,' \
		'agentops_ambient_contaminant-9.9.dist-info/RECORD,,' \
		>"${site_packages}/agentops_ambient_contaminant-9.9.dist-info/RECORD"
	write_inventory "${environment}" "${contaminated}"
	jq -e '.[] | select(.Name == "agentops-ambient-contaminant" and .License == "Proprietary")' \
		"${contaminated}" >/dev/null || fail "${label}: ambient contamination fixture was not observable"

	UV_PROJECT_ENVIRONMENT="${environment}" \
		uv sync \
		--project "${project}" \
		--locked \
		--no-install-project \
		--quiet \
		"${groups[@]}"
	write_inventory "${environment}" "${resynchronized}"
	if [[ -e ${site_packages}/agentops_ambient_contaminant-9.9.dist-info ]] || ! cmp -s "${output}" "${resynchronized}"; then
		fail "${label}: a pre-populated environment changed the lock-synchronized inventory"
	fi
	printf '%s dependencies: clean and pre-populated inventory verdicts are identical\n' "${label}"

	inventory_digest=$(sha256sum "${output}")
	inventory_digest=${inventory_digest%% *}
	printf '%s dependencies: lock-synchronized %s profile (%s)\n' \
		"${label}" \
		"${dependency_profile}" \
		"${inventory_digest}"
}

check_python_environment() {
	local label=$1
	local inventory_file=$2
	shift 2
	local ignored=" $* "
	local package_count
	local violations

	violations=$(
		jq -r \
			--arg ignored "${ignored}" \
			--argjson allowed "${allowed_licenses_json}" '
				.[] as $package |
				(" " + $package.Name + " ") as $needle |
				select(($ignored | contains($needle)) | not) |
				select(($allowed | index($package.License)) == null) |
				"\($package.Name) \($package.Version): \($package.License)"
			' "${inventory_file}"
	)
	if [[ -n ${violations} ]]; then
		printf '%s dependencies have unapproved license metadata:\n%s\n' "${label}" "${violations}" >&2
		return 1
	fi

	package_count=$(jq 'length' "${inventory_file}")
	printf '%s dependencies: %s packages use reviewed open-source licenses\n' \
		"${label}" \
		"${package_count}"
}

check_embedded_license() {
	local label=$1
	local inventory_file=$2
	local package=$3
	local expected=$4

	if ! jq -e --arg package "${package}" '[.[] | select(.Name == $package)] | length > 0' \
		"${inventory_file}" >/dev/null; then
		printf '%s: expected %s in this audited profile\n' "${label}" "${package}" >&2
		return 1
	fi

	if ! jq -e \
		--arg expected "${expected}" \
		--arg package "${package}" '
			[.[] | select(.Name == $package)] as $matches |
			($matches | length) == 1 and
			$matches[0].License == "UNKNOWN" and
			($matches[0].LicenseText | test($expected; "i"))
		' "${inventory_file}" >/dev/null; then
		printf '%s: could not verify the embedded license for %s\n' "${label}" "${package}" >&2
		return 1
	fi

	printf '%s: embedded license verified for %s\n' "${label}" "${package}"
}

check_repository_licenses
pids=()
sync_inventory "documentation" documentation . development "${inventory_dir}/documentation.json" &
pids+=("$!")
sync_inventory "agent runtime" agent-runtime agents/python runtime "${inventory_dir}/agent-runtime.json" &
pids+=("$!")
sync_inventory "agent development" agent-development agents/python development "${inventory_dir}/agent-development.json" &
pids+=("$!")
if [[ ${profile} == full ]]; then
	sync_inventory "agent evaluation" agent-evaluation agents/python evaluation "${inventory_dir}/agent-evaluation.json" &
	pids+=("$!")
	sync_inventory "MLflow runtime" mlflow-runtime infra/mlflow runtime "${inventory_dir}/mlflow.json" &
	pids+=("$!")
fi

inventory_failed=0
for pid in "${pids[@]}"; do
	wait "${pid}" || inventory_failed=1
done
if ((inventory_failed)); then
	exit 1
fi

check_python_environment "documentation" "${inventory_dir}/documentation.json"
check_python_environment "agent runtime" "${inventory_dir}/agent-runtime.json"
check_python_environment "agent development" "${inventory_dir}/agent-development.json" google-crc32c
check_embedded_license "agent development" "${inventory_dir}/agent-development.json" google-crc32c 'Apache License'
if [[ ${profile} == full ]]; then
	check_python_environment "agent evaluation" "${inventory_dir}/agent-evaluation.json" google-crc32c huey skops
	check_embedded_license "agent evaluation" "${inventory_dir}/agent-evaluation.json" google-crc32c 'Apache License'
	check_embedded_license "agent evaluation" "${inventory_dir}/agent-evaluation.json" huey 'Permission is hereby granted'
	check_embedded_license "agent evaluation" "${inventory_dir}/agent-evaluation.json" skops 'MIT License'
	check_python_environment "MLflow" "${inventory_dir}/mlflow.json" google-crc32c huey skops
	check_embedded_license "MLflow" "${inventory_dir}/mlflow.json" google-crc32c 'Apache License'
	check_embedded_license "MLflow" "${inventory_dir}/mlflow.json" huey 'Permission is hereby granted'
	check_embedded_license "MLflow" "${inventory_dir}/mlflow.json" skops 'MIT License'
fi
