#!/usr/bin/env bash
# Manage SOPS + age encrypted Secret manifests for the course (Ch. 6.5).
# Ciphertext under infra/**/secrets/ is safe to commit; the age private key is
# lab material that stays gitignored under infra/secrets/ — never commit it.
# This is a lab pattern (one local key), not a production KMS with rotation.
#
# Usage: infra/scripts/secrets.sh <command> [file]
#   keygen          generate the gitignored age key and print its public recipient
#   encrypt <file>  encrypt data/stringData in place per the root .sops.yaml rule
#   decrypt <file>  print the decrypted manifest on stdout (pipe to kubectl apply)
#   edit <file>     edit the encrypted manifest through sops with $EDITOR

set -Eeuo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
key_file="infra/secrets/age.agekey"
# Honor an explicitly exported SOPS_AGE_KEY_FILE; otherwise default to the
# gitignored lab key so keygen/decrypt/edit work without extra setup.
export SOPS_AGE_KEY_FILE="${SOPS_AGE_KEY_FILE:-${PWD}/${key_file}}"

command="${1:-}"
file="${2:-}"

require_file() {
	if [[ ! -f "${file}" ]]; then
		echo "Usage: infra/scripts/secrets.sh ${command} <file>" >&2
		exit 1
	fi
}

case "${command}" in
keygen)
	if [[ -f "${key_file}" && "${file}" != "--force" ]]; then
		echo "Key already present at ${key_file} (use --force to overwrite)." >&2
	else
		mkdir -p "$(dirname "${key_file}")"
		rm -f "${key_file}"
		age-keygen -o "${key_file}"
	fi
	recipient="$(age-keygen -y "${key_file}")"
	echo "Public recipient: ${recipient}" >&2
	echo "Next: put this recipient in .sops.yaml, then encrypt your own manifests." >&2
	echo "The private key at ${key_file} is gitignored — never commit or share it." >&2
	;;
encrypt)
	require_file
	sops encrypt --in-place "${file}"
	echo "Encrypted ${file} in place (data/stringData values only)." >&2
	;;
decrypt)
	require_file
	sops decrypt "${file}"
	;;
edit)
	require_file
	sops edit "${file}"
	;;
*)
	echo "Usage: infra/scripts/secrets.sh <keygen|encrypt|decrypt|edit> [file]" >&2
	exit 1
	;;
esac
