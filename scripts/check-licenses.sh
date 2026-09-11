#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd cmp base
require_cmd rg base
require_cmd trivy base

profile=${1:-full}
readonly profile
if (($# > 1)); then
	fail "usage: $0 [core|full]"
fi
case "${profile}" in
core | full) ;;
*) fail "unknown license profile '${profile}'; expected core or full" ;;
esac

check_repository_licenses() {
	local software_license
	local font_license=static/assets/fmind/OFL-1.1.txt

	test -f LICENSE
	test -f static/LICENSE.txt
	for software_license in agents/LICENSE clients/LICENSE evals/LICENSE infra/LICENSE load/LICENSE tools/LICENSE; do
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

check_repository_licenses
# One scanner now owns every Go module, container manifest, and vendored asset. Keeping the
# dependency verdict in Trivy avoids rebuilding language-specific inventories in repository code.
"${lib_dir}/trivy-repository.sh" licenses
