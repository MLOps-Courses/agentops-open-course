#!/usr/bin/env bash

set -Eeuo pipefail

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/agentops-docs.XXXXXX")
readonly tmp_dir
trap 'rm -rf -- "${tmp_dir}"' EXIT

pages_file="${tmp_dir}/pages"
headings_file="${tmp_dir}/headings"
rg --files docs -g '*.md' | sort >"${pages_file}"

failed=0

while IFS= read -r page; do
	first_line=$(sed -n '1p' "${page}")
	second_line=$(sed -n '2p' "${page}")
	if [[ ${first_line} != '---' ]] || [[ ! ${second_line} =~ ^description:\ .+ ]]; then
		printf '%s: expected description front matter at the start of the file\n' "${page}" >&2
		failed=1
	fi

	heading_count=0
	awk '
		/^(```|~~~)/ { in_fence = !in_fence; next }
		!in_fence && /^## / { print }
	' "${page}" >"${headings_file}"
	while IFS= read -r heading; do
		heading_count=$((heading_count + 1))
		if [[ ${heading} != *'?' ]]; then
			printf '%s: FAQ heading must end with ?: %s\n' "${page}" "${heading}" >&2
			failed=1
		fi
	done <"${headings_file}"
	if ((heading_count == 0)); then
		printf '%s: expected at least one FAQ question heading\n' "${page}" >&2
		failed=1
	fi
done <"${pages_file}"

if rg -n '/home/[^ /]+|file:///|k3d-registry\.localhost' docs; then
	printf 'docs: found a machine-specific path or obsolete registry hostname\n' >&2
	failed=1
fi

exit "${failed}"
