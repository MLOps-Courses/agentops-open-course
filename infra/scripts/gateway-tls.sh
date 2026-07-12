#!/usr/bin/env bash
# Generate a DEMO-ONLY self-signed TLS certificate for the secured host
# gateway profile (infra/agentgateway/host/config-auth.yaml). Lab trust only:
# clients must pass the certificate explicitly (curl --cacert). The output
# directory is gitignored — never commit certificates or private keys.
#
# Usage: infra/scripts/gateway-tls.sh [--force]

set -Eeuo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
out_dir="infra/agentgateway/host/auth"
cert_file="${out_dir}/tls-cert.pem"
key_file="${out_dir}/tls-key.pem"

mkdir -p "${out_dir}"

if [[ -f "${cert_file}" && -f "${key_file}" && "${1:-}" != "--force" ]]; then
	echo "TLS material already present in ${out_dir} (use --force to regenerate)." >&2
	exit 0
fi

openssl req -x509 -newkey rsa:2048 -sha256 -days 30 -nodes \
	-keyout "${key_file}" -out "${cert_file}" \
	-subj "/CN=localhost" \
	-addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null

echo "Wrote ${cert_file} and ${key_file} (demo-only, 30 days)." >&2
