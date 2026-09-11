#!/usr/bin/env bash

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# Every assertion below is an `rg` match, so a host without ripgrep would report a
# lost reviewed digest rather than a missing tool. Name the real cause up front.
require_cmd rg base

installer="$(dirname "${BASH_SOURCE[0]}")/install-helm-diff.sh"
installed_version="$("${installer}" --version)"
assert_eq "helm-diff installer version" "${installed_version}" "3.15.10"

for digest in \
	4128d6059d4dbeed97a1a67b53a8c621d90a2854a4688fd1e7f98e54bcd57f85 \
	798e9bee615a5ea3159e2cb1edc01d1c3a9dad689e8a3a2e7c9ed9e2e59dc0ea \
	9f6baef5b1a8814ed3bbf14221cccf454e71e2889541c8a4e4c924a9f7a0aa9f \
	794f100046869f00fd1872e1cf724c19ad908b38bb6c6ba144cd82b6ffa0a0e9; do
	rg -Fq "${digest}" "${installer}" || fail "helm-diff installer lost reviewed executable digest ${digest}"
done
rg -Fq 'verify_git_binary_install' "${installer}" ||
	fail "helm-diff installer no longer verifies an existing plugin's source and executable"
