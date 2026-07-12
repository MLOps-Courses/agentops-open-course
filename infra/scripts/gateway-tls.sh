#!/usr/bin/env bash
# Generate a DEMO-ONLY local CA and server certificate for the secured host
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
ca_cert_file="${out_dir}/ca-cert.pem"
ca_key_file="${out_dir}/ca-key.pem"
csr_file="${out_dir}/tls-cert.csr"

mkdir -p "${out_dir}"

if [[ -f "${ca_cert_file}" && -f "${ca_key_file}" && -f "${cert_file}" && -f "${key_file}" && "${1:-}" != "--force" ]]; then
	echo "TLS material already present in ${out_dir} (use --force to regenerate)." >&2
	exit 0
fi

openssl req -x509 -newkey rsa:2048 -sha256 -days 30 -nodes \
	-keyout "${ca_key_file}" -out "${ca_cert_file}" \
	-subj "/CN=AgentOps Course Demo CA" \
	-addext "basicConstraints=critical,CA:TRUE" \
	-addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

openssl req -new -newkey rsa:2048 -sha256 -nodes \
	-keyout "${key_file}" -out "${csr_file}" \
	-subj "/CN=localhost" \
	-addext "basicConstraints=critical,CA:FALSE" \
	-addext "keyUsage=critical,digitalSignature,keyEncipherment" \
	-addext "extendedKeyUsage=serverAuth" \
	-addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null

openssl x509 -req -sha256 -days 30 \
	-in "${csr_file}" -CA "${ca_cert_file}" -CAkey "${ca_key_file}" -CAcreateserial \
	-copy_extensions copy -out "${cert_file}" 2>/dev/null
rm -f "${csr_file}" "${out_dir}/ca-cert.srl"

echo "Wrote local CA ${ca_cert_file} and server certificate ${cert_file} (demo-only, 30 days)." >&2
