#!/usr/bin/env bash
# Mint a DEMO-ONLY RS256 JWT for the secured host gateway profile
# (infra/agentgateway/host/config-auth.yaml). The first run generates a local
# signing key plus its public JWKS. The output directory is gitignored — the
# signing key is lab material, never commit it or reuse it anywhere else.
#
# Usage: infra/scripts/gateway-jwt.sh [subject] [ttl-seconds]
#   subject      JWT `sub` claim: ops-admin (default) or ops-viewer
#   ttl-seconds  token lifetime (default: 3600)

set -Eeuo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
out_dir="infra/agentgateway/host/auth"
key_file="${out_dir}/jwt-signing-key.pem"
jwks_file="${out_dir}/jwks.json"
subject="${1:-ops-admin}"
ttl="${2:-3600}"

mkdir -p "${out_dir}"

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

if [[ ! -f "${key_file}" ]]; then
	openssl genrsa -out "${key_file}" 2048 2>/dev/null
	echo "Generated demo signing key ${key_file}." >&2
fi

# The JWKS carries only the public modulus/exponent, so it is safe to share;
# it is regenerated whenever the signing key changes to stay in sync.
if [[ ! -f "${jwks_file}" || "${key_file}" -nt "${jwks_file}" ]]; then
	modulus="$(openssl rsa -in "${key_file}" -noout -modulus | cut -d= -f2 | xxd -r -p | b64url)"
	printf '{"keys":[{"kty":"RSA","alg":"RS256","use":"sig","kid":"agentops-demo","n":"%s","e":"AQAB"}]}\n' \
		"${modulus}" >"${jwks_file}"
	echo "Wrote public JWKS ${jwks_file}." >&2
fi

now="$(date +%s)"
header='{"alg":"RS256","typ":"JWT","kid":"agentops-demo"}'
payload="$(printf '{"iss":"agentops-course","aud":"agentops-gateway","sub":"%s","iat":%s,"exp":%s}' \
	"${subject}" "${now}" "$((now + ttl))")"
header_b64="$(printf '%s' "${header}" | b64url)"
payload_b64="$(printf '%s' "${payload}" | b64url)"
signing_input="${header_b64}.${payload_b64}"
signature="$(printf '%s' "${signing_input}" | openssl dgst -sha256 -sign "${key_file}" -binary | b64url)"
echo "${signing_input}.${signature}"
