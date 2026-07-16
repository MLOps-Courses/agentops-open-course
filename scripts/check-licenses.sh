#!/usr/bin/env bash

set -Eeuo pipefail

readonly allowed_licenses_json='[
  "3-Clause BSD License",
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

	test -f LICENSE
	test -f docs/LICENSE.txt
	for software_license in agents/LICENSE clients/LICENSE infra/LICENSE load/LICENSE; do
		if ! cmp -s LICENSE "${software_license}"; then
			printf '%s: software license differs from the root MIT license\n' "${software_license}" >&2
			return 1
		fi
	done

	printf 'repository licenses: MIT software + CC BY 4.0 course content\n'
}

collect_inventory() {
	local label=$1
	local python=$2
	local output=$3

	if [[ ! -x ${python} ]]; then
		printf '%s: missing environment; run mise run install\n' "${python}" >&2
		return 1
	fi
	if [[ ! -x ${pip_licenses} ]]; then
		printf '%s: missing license checker; run mise run install\n' "${pip_licenses}" >&2
		return 1
	fi

	"${pip_licenses}" \
		--python "${python}" \
		--from mixed \
		--with-license-file \
		--no-license-path \
		--format json \
		>"${output}"
	printf '%s dependencies: inventory collected\n' "${label}"
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
collect_inventory "documentation" .venv/bin/python "${inventory_dir}/documentation.json" &
documentation_pid=$!
collect_inventory "agent" agents/python/.venv/bin/python "${inventory_dir}/agent.json" &
agent_pid=$!
collect_inventory "MLflow" infra/mlflow/.venv/bin/python "${inventory_dir}/mlflow.json" &
mlflow_pid=$!

inventory_failed=0
wait "${documentation_pid}" || inventory_failed=1
wait "${agent_pid}" || inventory_failed=1
wait "${mlflow_pid}" || inventory_failed=1
if ((inventory_failed)); then
	exit 1
fi

check_python_environment "documentation" "${inventory_dir}/documentation.json"
check_python_environment "agent" "${inventory_dir}/agent.json" huey skops
check_python_environment "MLflow" "${inventory_dir}/mlflow.json" google-crc32c huey skops
check_embedded_license "agent" "${inventory_dir}/agent.json" huey 'Permission is hereby granted'
check_embedded_license "agent" "${inventory_dir}/agent.json" skops 'MIT License'
check_embedded_license "MLflow" "${inventory_dir}/mlflow.json" google-crc32c 'Apache License'
check_embedded_license "MLflow" "${inventory_dir}/mlflow.json" huey 'Permission is hereby granted'
check_embedded_license "MLflow" "${inventory_dir}/mlflow.json" skops 'MIT License'
