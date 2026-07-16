#!/usr/bin/env bash

set -Eeuo pipefail

readonly expected=3.15.10
readonly expected_commit=5873f8d94712f014dc2bb329acae63b8ffbf569b
current=$(helm plugin list | awk '$1 == "diff" { print $2 }')

if [[ -z ${current} ]]; then
	kernel=$(uname -s)
	machine=$(uname -m)
	platform="${kernel}-${machine}"
	case "${platform}" in
	Linux-x86_64)
		asset=helm-diff-linux-amd64.tgz
		expected_sha256=ffeff863e4a3cbe83282a13a55ee972f7497966dfb66f326a117f9b094fff161
		;;
	Linux-aarch64 | Linux-arm64)
		asset=helm-diff-linux-arm64.tgz
		expected_sha256=6832085986feef54b7be82906f2516f3565fdd1be11c109737e8833f1e1c0a5c
		;;
	Darwin-x86_64)
		asset=helm-diff-macos-amd64.tgz
		expected_sha256=5d1ae1d4cfdc138612ec99faac5ffa1251306e8c66dfad2cdeb7c4457f3dd875
		;;
	Darwin-arm64)
		asset=helm-diff-macos-arm64.tgz
		expected_sha256=a53fc7515226e071c748a19cd6d3f7b490e2cfd7301cb6e1ce02e6fee19d54cf
		;;
	*)
		printf 'helm-diff: unsupported platform %s; install version %s manually\n' \
			"${platform}" "${expected}" >&2
		exit 1
		;;
	esac

	tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/helm-diff.XXXXXX")
	trap 'rm -rf -- "${tmp_dir}"' EXIT
	archive="${tmp_dir}/${asset}"
	curl --fail --show-error --location --retry 3 \
		--output "${archive}" \
		"https://github.com/databus23/helm-diff/releases/download/v${expected}/${asset}"
	if command -v sha256sum >/dev/null 2>&1; then
		actual_sha256=$(sha256sum "${archive}" | awk '{ print $1 }')
	else
		actual_sha256=$(shasum -a 256 "${archive}" | awk '{ print $1 }')
	fi
	if [[ ${actual_sha256} != "${expected_sha256}" ]]; then
		printf 'helm-diff: checksum mismatch for %s (expected %s, got %s)\n' \
			"${asset}" "${expected_sha256}" "${actual_sha256}" >&2
		exit 1
	fi

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
