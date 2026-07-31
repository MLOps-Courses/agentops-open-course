#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

tmp_dir=$(mktemp -d)
trap 'rm -r -- "${tmp_dir}"' EXIT

mkdir "${tmp_dir}/v1" "${tmp_dir}/v2"
touch "${tmp_dir}/v2/cgroup.controllers"

require_cgroup_v2 "${tmp_dir}/v2"
if (require_cgroup_v2 "${tmp_dir}/v1") 2>"${tmp_dir}/error"; then
	fail "require_cgroup_v2 accepted a cgroup v1 hierarchy"
fi
grep -Fqx \
	"cgroup v2 required for pinned Kubernetes; enable the unified cgroup hierarchy before running local k3d" \
	"${tmp_dir}/error"
