#!/usr/bin/env bash

set -Eeuo pipefail

pages_output=$(rg --files docs -g '*.md' | sort)
mapfile -t pages <<<"${pages_output}"
failed=0

for page in "${pages[@]}"; do
	first_line=$(sed -n '1p' "${page}")
	second_line=$(sed -n '2p' "${page}")
	if [[ ${first_line} != '---' ]] || [[ ! ${second_line} =~ ^description:\ .+ ]]; then
		printf '%s: expected description front matter at the start of the file\n' "${page}" >&2
		failed=1
	fi

	headings_output=$(awk '
			/^(```|~~~)/ { in_fence = !in_fence; next }
			!in_fence && /^## / { print }
		' "${page}")
	if [[ -n ${headings_output} ]]; then
		mapfile -t headings <<<"${headings_output}"
	else
		headings=()
	fi
	if ((${#headings[@]} == 0)); then
		printf '%s: expected at least one FAQ question heading\n' "${page}" >&2
		failed=1
		continue
	fi

	for heading in "${headings[@]}"; do
		if [[ ${heading} != *'?' ]]; then
			printf '%s: FAQ heading must end with ?: %s\n' "${page}" "${heading}" >&2
			failed=1
		fi
	done
done

if rg -n '/home/[^ /]+|file:///|k3d-registry\.localhost' docs; then
	printf 'docs: found a machine-specific path or obsolete registry hostname\n' >&2
	failed=1
fi

exit "${failed}"
