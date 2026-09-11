#!/usr/bin/env bash

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# Every assertion below is an `rg` match, so a host without ripgrep would report a
# corrupted installer rather than a missing tool. Name the real cause up front.
require_cmd rg base

installer="$(dirname "${BASH_SOURCE[0]}")/install-sqlite.sh"
installed_version="$("${installer}" --version)"
assert_eq "SQLite installer version" "${installed_version}" "3.53.4"

rg -Fq 'sqlite-autoconf-3530400.tar.gz' "${installer}" ||
	fail "SQLite installer lost the exact official archive"
rg -Fq '0e9483900e92cd5de8fd48d16bf9200145a61f7fd5be542a5ac81d8a9516eb9c' "${installer}" ||
	fail "SQLite installer lost the reviewed archive digest"
