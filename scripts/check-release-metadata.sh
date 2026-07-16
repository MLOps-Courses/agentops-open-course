#!/usr/bin/env bash

set -Eeuo pipefail

expected_tag="${1:-}"

project_version() {
	awk '
		$0 == "[project]" {
			in_project = 1
			next
		}
		in_project && /^\[/ {
			exit
		}
		in_project && /^version = "[^"]+"$/ {
			value = $0
			sub(/^version = "/, "", value)
			sub(/"$/, "", value)
			print value
			exit
		}
	' "$1"
}

cff_value() {
	local key="$1"

	awk -v key="${key}" '
		index($0, key ":") == 1 {
			value = substr($0, length(key) + 2)
			sub(/^[[:space:]]*/, "", value)
			gsub(/^"|"$/, "", value)
			print value
			exit
		}
	' CITATION.cff
}

root_version="$(project_version pyproject.toml)"
agent_version="$(project_version agents/python/pyproject.toml)"
citation_version="$(cff_value version)"
citation_date="$(cff_value date-released)"

for value_name in root_version agent_version citation_version citation_date; do
	if [[ -z "${!value_name}" ]]; then
		echo "release metadata: could not read ${value_name}" >&2
		exit 1
	fi
done

if [[ ! "${root_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "release metadata: root version is not stable SemVer: ${root_version}" >&2
	exit 1
fi
if [[ "${agent_version}" != "${root_version}" || "${citation_version}" != "${root_version}" ]]; then
	printf 'release metadata: version mismatch (root=%s agent=%s citation=%s)\n' \
		"${root_version}" "${agent_version}" "${citation_version}" >&2
	exit 1
fi
if [[ ! "${citation_date}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
	echo "release metadata: CITATION.cff date-released is not YYYY-MM-DD: ${citation_date}" >&2
	exit 1
fi

expected_heading="## [${root_version}] - ${citation_date}"
first_release_heading="$(awk '/^## \[[0-9]+\.[0-9]+\.[0-9]+\] - / { print; exit }' CHANGELOG.md)"
if [[ "${first_release_heading}" != "${expected_heading}" ]]; then
	printf 'release metadata: newest changelog heading must be %q, got %q\n' \
		"${expected_heading}" "${first_release_heading}" >&2
	exit 1
fi

expected_link="[${root_version}]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v${root_version}"
if ! grep -Fxq "${expected_link}" CHANGELOG.md; then
	echo "release metadata: CHANGELOG.md is missing ${expected_link}" >&2
	exit 1
fi

if [[ -n "${expected_tag}" && "${expected_tag}" != "v${root_version}" ]]; then
	printf 'release metadata: tag %s does not match source version v%s\n' \
		"${expected_tag}" "${root_version}" >&2
	exit 1
fi

echo "release metadata: v${root_version} (${citation_date}) is consistent"
