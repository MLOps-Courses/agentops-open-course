#!/usr/bin/env bash

# Syntax-check every inline mise task body with the shell that will actually run
# it.
#
# mise executes an inline `run` body with `sh -c -o errexit` unless the task
# names its own `shell`, and `/bin/sh` is dash on Debian-family hosts. A body
# written in bash (`<<<`, `[[ ]]`, arrays) therefore fails at run time with
# `Syntax error: redirection unexpected` while every other gate stays green,
# because the lint step reads `scripts/*.sh` and `infra/scripts/*.sh` only and
# never sees a task body.
# `unix_default_inline_shell_args` cannot fix it either — mise ignores that
# setting outside a global config, so a task must declare its own `shell`.

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd jq base
require_cmd mise base

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_dir}" || exit

tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${tmp_dir}"' EXIT
body_file="${tmp_dir}/body.sh"

# Every directory that owns a mise.toml. Listing tasks inside a module also
# returns the inherited root tasks, so each root keeps only the tasks it defines.
task_roots=(. agents/data agents/go evals tools)

checked=0
for task_root in "${task_roots[@]}"; do
	config="${repo_dir}/${task_root#./}/mise.toml"
	config="${config//\/.\///}"
	[[ -f ${config} ]] || fail "${config} is missing; update the task-root list"

	tasks_json="$(cd "${task_root}" && mise tasks ls --json --hidden)"
	owned_json="$(jq --arg config "${config}" '[.[] | select(.source == $config)]' <<<"${tasks_json}")"
	task_count="$(jq -er 'length' <<<"${owned_json}")"
	((task_count > 0)) || fail "${config} declares no tasks; update the task-root list"

	for ((index = 0; index < task_count; index++)); do
		task_json="$(jq -er --argjson index "${index}" '.[$index]' <<<"${owned_json}")"
		name="$(jq -er '.name' <<<"${task_json}")"

		# An absent `shell` means mise's default inline shell, not "no shell".
		interpreter='sh'
		shell_spec="$(jq -r '.shell // ""' <<<"${task_json}")"
		[[ -z ${shell_spec} ]] || read -r interpreter _ <<<"${shell_spec}"
		command -v "${interpreter}" >/dev/null 2>&1 ||
			fail "${name} names shell ${interpreter}, which is not installed"

		body_count="$(jq -er '.run | length' <<<"${task_json}")"
		for ((body_index = 0; body_index < body_count; body_index++)); do
			jq -er --argjson index "${body_index}" '.run[$index]' <<<"${task_json}" >"${body_file}"
			"${interpreter}" -n "${body_file}" ||
				fail "task ${name} is not valid ${interpreter} syntax; declare shell = \"bash -c -o errexit\" or rewrite the body"
			checked=$((checked + 1))
		done
	done
done

printf '%d inline mise task bodies parse under their configured shell\n' "${checked}"
