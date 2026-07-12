#!/usr/bin/env bash

set -Eeuo pipefail

expected=3.15.10
current=$(helm plugin list | awk '$1 == "diff" { print $2 }')

if [[ -z ${current} ]]; then
	# helm-diff is a legacy Helm plugin and does not publish Helm 4 verification
	# metadata. Keep the exception local to this exact reviewed release.
	helm plugin install https://github.com/databus23/helm-diff --version "v${expected}" --verify=false
elif [[ ${current} != "${expected}" ]]; then
	printf 'helm-diff: expected %s, found %s; reconcile the shared Helm plugin before continuing\n' \
		"${expected}" "${current}" >&2
	exit 1
fi
