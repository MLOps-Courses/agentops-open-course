#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

readonly expected=3.15.10

if [[ ${1:-} == --version ]]; then
	printf '%s\n' "${expected}"
	exit 0
fi

require_host_cmd curl "install curl from a reviewed host package source"
require_host_cmd git "install Git from a reviewed host package source"
require_cmd helm platform

readonly expected_commit=5873f8d94712f014dc2bb329acae63b8ffbf569b
kernel=$(uname -s)
machine=$(uname -m)
platform="${kernel}-${machine}"
case "${platform}" in
Linux-x86_64)
	asset=helm-diff-linux-amd64.tgz
	expected_sha256=ffeff863e4a3cbe83282a13a55ee972f7497966dfb66f326a117f9b094fff161
	expected_binary_sha256=4128d6059d4dbeed97a1a67b53a8c621d90a2854a4688fd1e7f98e54bcd57f85
	;;
Linux-aarch64 | Linux-arm64)
	asset=helm-diff-linux-arm64.tgz
	expected_sha256=6832085986feef54b7be82906f2516f3565fdd1be11c109737e8833f1e1c0a5c
	expected_binary_sha256=798e9bee615a5ea3159e2cb1edc01d1c3a9dad689e8a3a2e7c9ed9e2e59dc0ea
	;;
Darwin-x86_64)
	asset=helm-diff-macos-amd64.tgz
	expected_sha256=5d1ae1d4cfdc138612ec99faac5ffa1251306e8c66dfad2cdeb7c4457f3dd875
	expected_binary_sha256=9f6baef5b1a8814ed3bbf14221cccf454e71e2889541c8a4e4c924a9f7a0aa9f
	;;
Darwin-arm64)
	asset=helm-diff-macos-arm64.tgz
	expected_sha256=a53fc7515226e071c748a19cd6d3f7b490e2cfd7301cb6e1ce02e6fee19d54cf
	expected_binary_sha256=794f100046869f00fd1872e1cf724c19ad908b38bb6c6ba144cd82b6ffa0a0e9
	;;
*)
	printf 'helm-diff: unsupported platform %s; install version %s manually\n' \
		"${platform}" "${expected}" >&2
	exit 1
	;;
esac

current=$(helm plugin list | awk '$1 == "diff" { print $2 }')

if [[ -z ${current} ]]; then

	tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/helm-diff.XXXXXX")
	trap 'rm -rf -- "${tmp_dir}"' EXIT
	archive="${tmp_dir}/${asset}"
	curl --fail --show-error --location --retry 3 \
		--output "${archive}" \
		"https://github.com/databus23/helm-diff/releases/download/v${expected}/${asset}"
	verify_sha256 "${archive}" "${expected_sha256}" "helm-diff ${asset}"

	# helm-diff is a legacy Helm plugin and does not publish Helm 4 verification
	# metadata. Pin both the source commit and platform asset checksum, and keep
	# the verification exception local to this one reviewed install.
	HELM_DIFF_BIN_TGZ="${archive}" helm plugin install https://github.com/databus23/helm-diff \
		--version "${expected_commit}" \
		--verify=false
	current=$(helm plugin list | awk '$1 == "diff" { print $2 }')
	if [[ ${current} != "${expected}" ]]; then
		printf 'helm-diff: commit %s reported version %s instead of %s\n' \
			"${expected_commit}" "${current:-missing}" "${expected}" >&2
		exit 1
	fi
elif [[ ${current} != "${expected}" ]]; then
	printf 'helm-diff: expected %s, found %s; reconcile the shared Helm plugin before continuing\n' \
		"${expected}" "${current}" >&2
	exit 1
fi

plugin_root="$(helm env HELM_PLUGINS)"
[[ -n ${plugin_root} ]] || fail "helm-diff: Helm returned an empty plugin directory"
verify_git_binary_install \
	"${plugin_root}/helm-diff" "${expected_commit}" "bin/diff" "${expected_binary_sha256}" "helm-diff ${expected}"
