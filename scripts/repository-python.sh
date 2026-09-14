#!/usr/bin/env bash

# Run the agent-owned Python toolchain over repository Python outside agents/python.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib.sh
source "${script_dir}/lib.sh"

require_cmd uv base

declare -a files=()
while IFS= read -r path; do
	[[ -n "${path}" ]] || continue
	files+=("../../${path}")
done <"${script_dir}/repository-python-files.txt"
((${#files[@]} > 0)) || fail "repository Python inventory is empty"

cd "${repo_dir}/agents/python"
case "${1:-}" in
format)
	uv run ruff check --select=I --fix "${files[@]}"
	uv run ruff format "${files[@]}"
	;;
check)
	uv run ruff format --check "${files[@]}"
	uv run ruff check "${files[@]}"
	uv run ty check --extra-search-path ../.. "${files[@]}"
	;;
*)
	fail "usage: repository-python.sh <format|check>"
	;;
esac
