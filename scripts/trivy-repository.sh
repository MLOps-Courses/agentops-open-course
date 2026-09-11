#!/usr/bin/env bash

set -euo pipefail

readonly mode=${1:-}
readonly -a common=(
	--config trivy.yaml
	--exit-code 1
	--skip-dirs .agents
	--skip-dirs .cache
	--skip-dirs .git
	--skip-dirs site
	--skip-dirs infra/agentgateway/host/auth
)

case "${mode}" in
source)
	trivy fs "${common[@]}" \
		--scanners vuln,misconfig,secret \
		--tf-vars infra/gcp/terraform.tfvars.example \
		--skip-files .env \
		.
	;;
licenses)
	trivy fs "${common[@]}" \
		--scanners license \
		--severity UNKNOWN,HIGH,CRITICAL \
		--skip-files .env \
		.
	;;
config)
	trivy config "${common[@]}" \
		--tf-vars infra/gcp/terraform.tfvars.example \
		.
	;;
*)
	printf 'usage: %s {source|licenses|config}\n' "$0" >&2
	exit 2
	;;
esac
