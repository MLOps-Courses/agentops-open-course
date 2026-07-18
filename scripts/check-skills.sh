#!/usr/bin/env bash

# Validate the installable Agent Skills under skills/: every skill directory must
# hold a SKILL.md with `name` and `description` front matter and an H1 title, the
# name must match its directory (so `npx skills add --skill <dir>` resolves), and
# no skill may embed a machine-specific path. Portable-guidance skills only.

set -Eeuo pipefail

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/agentops-skills.XXXXXX")
readonly tmp_dir
trap 'rm -rf -- "${tmp_dir}"' EXIT

skills_list="${tmp_dir}/skills"
find skills -mindepth 2 -maxdepth 2 -name SKILL.md | sort >"${skills_list}"

if [[ ! -s ${skills_list} ]]; then
	printf 'skills: no SKILL.md found under skills/\n' >&2
	exit 1
fi

failed=0

while IFS= read -r skill_file; do
	directory=$(basename "$(dirname "${skill_file}")")

	# Front matter is the first block delimited by --- ... --- at the top of the file.
	first_line=$(head -n1 "${skill_file}")
	if [[ ${first_line} != '---' ]]; then
		printf '%s: must start with YAML front matter (---)\n' "${skill_file}" >&2
		failed=1
		continue
	fi
	front_matter=$(awk 'NR==1{next} /^---[[:space:]]*$/{exit} {print}' "${skill_file}")
	name=$(printf '%s\n' "${front_matter}" | sed -n 's/^name:[[:space:]]*//p' | head -n1)
	description=$(printf '%s\n' "${front_matter}" | sed -n 's/^description:[[:space:]]*//p' | head -n1)

	if [[ -z ${name} ]]; then
		printf '%s: front matter is missing a name\n' "${skill_file}" >&2
		failed=1
	elif [[ ${name} != "${directory}" ]]; then
		printf '%s: name %q must match its directory %q\n' "${skill_file}" "${name}" "${directory}" >&2
		failed=1
	fi

	if [[ -z ${description} ]]; then
		printf '%s: front matter is missing a description\n' "${skill_file}" >&2
		failed=1
	fi

	if ! grep -qE '^# ' "${skill_file}"; then
		printf '%s: expected an H1 title\n' "${skill_file}" >&2
		failed=1
	fi

	if grep -qE '/home/[^ /]+|file:///' "${skill_file}"; then
		printf '%s: found a machine-specific path (skills must be portable)\n' "${skill_file}" >&2
		failed=1
	fi
done <"${skills_list}"

exit "${failed}"
