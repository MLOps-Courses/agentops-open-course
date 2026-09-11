#!/usr/bin/env bash

# Build the exact SQLite CLI used to reproduce agents/data/incidents.db.
# The legacy asdf plugin fetched moving code and unchecked source; this script
# owns one reviewed upstream archive and reconstructs the generated binary on
# every install instead of trusting an existing executable by version string.

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

readonly expected_version=3.53.4
readonly archive_name=sqlite-autoconf-3530400.tar.gz
readonly archive_url="https://www.sqlite.org/2026/${archive_name}"
# Independently downloaded from the official immutable URL; its SHA3-256 also
# matches SQLite's published PRODUCT record for 3.53.4.
readonly expected_sha256=0e9483900e92cd5de8fd48d16bf9200145a61f7fd5be542a5ac81d8a9516eb9c

if [[ ${1:-} == --version ]]; then
	printf '%s\n' "${expected_version}"
	exit 0
fi
[[ $# == 0 ]] || fail "usage: $0 [--version]"

require_host_cmd curl "install curl from a reviewed host package source"
require_host_cmd cc "install a C compiler from a reviewed host package source"
require_host_cmd install "install a POSIX-compatible install utility from a reviewed host package source"
require_host_cmd make "install make from a reviewed host package source"
require_host_cmd tar "install tar from a reviewed host package source"

repo_root="$(cd -- "${lib_dir}/.." && pwd)"
readonly repo_root
readonly target_dir="${repo_root}/.agents/bin"
readonly target="${target_dir}/sqlite3"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/agentops-sqlite.XXXXXX")"
case "${tmp_dir}" in
"${TMPDIR:-/tmp}"/agentops-sqlite.*) ;;
*) fail "unexpected SQLite build directory: ${tmp_dir}" ;;
esac
trap '[[ -d ${tmp_dir} && ! -L ${tmp_dir} ]] && rm -rf -- "${tmp_dir}"' EXIT

archive="${tmp_dir}/${archive_name}"
source_dir="${tmp_dir}/source"
prefix="${tmp_dir}/install"
mkdir -p "${source_dir}" "${prefix}" "${target_dir}"
curl --fail --show-error --location --retry 3 --output "${archive}" "${archive_url}"
verify_sha256 "${archive}" "${expected_sha256}" "SQLite ${expected_version} source archive"
tar -xzf "${archive}" --strip-components=1 -C "${source_dir}"

jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null || printf '1')"
[[ ${jobs} =~ ^[1-9][0-9]*$ ]] || jobs=1
(
	cd "${source_dir}"
	./configure --disable-shared --enable-static --prefix="${prefix}"
	make -j "${jobs}" sqlite3
)
install -m 0755 "${source_dir}/sqlite3" "${target}"
version_output="$("${target}" -init /dev/null --version)"
installed_version="$(awk '{ print $1 }' <<<"${version_output}")"
assert_eq "installed SQLite version" "${installed_version}" "${expected_version}"
