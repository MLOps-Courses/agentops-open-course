#!/usr/bin/env bash
# Re-pin the self-hosted browser bundles under assets/js/vendor/.
#
# Hextra loads Mermaid and FlexSearch from jsDelivr by default. The course self-hosts its
# assets, so both bundles are vendored and their versions and digests recorded in
# versions.json, which the native conventions source check verifies on every run.
#
# Usage: scripts/vendor-assets.sh [mermaid-version] [flexsearch-version]
#
# Nothing watches these two versions: they are not mise pins, so the freshness report
# never sees them, and they are not npm dependencies, so Dependabot never sees them
# either. Re-check them here, by hand, when the quarterly freshness issue is triaged:
#
#     curl -fsSL https://registry.npmjs.org/mermaid/latest    | jq -r .version
#     curl -fsSL https://registry.npmjs.org/flexsearch/latest | jq -r .version
#
# Checked on 11 September 2026: flexsearch 0.8.212 is current. Mermaid 12.0.0 is
# available and deliberately not taken — a major that changes diagram rendering would
# have to be read against all 55 diagrams and the accessibility acceptance run, which
# is a reviewed change rather than a re-pin.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendor="${root}/assets/js/vendor"
manifest="${vendor}/versions.json"

mermaid_version="${1:-$(jq -r '."mermaid.min.js".version' "${manifest}")}"
flexsearch_version="${2:-$(jq -r '."flexsearch.bundle.min.js".version' "${manifest}")}"

mermaid_url="https://cdn.jsdelivr.net/npm/mermaid@${mermaid_version}/dist/mermaid.min.js"
flexsearch_url="https://cdn.jsdelivr.net/npm/flexsearch@${flexsearch_version}/dist/flexsearch.bundle.min.js"

mkdir -p "${vendor}"
curl -fsSL --proto '=https' --tlsv1.2 -o "${vendor}/mermaid.min.js" "${mermaid_url}"
curl -fsSL --proto '=https' --tlsv1.2 -o "${vendor}/flexsearch.bundle.min.js" "${flexsearch_url}"

mermaid_sha256="$(sha256sum "${vendor}/mermaid.min.js")"
mermaid_sha256="${mermaid_sha256%% *}"
flexsearch_sha256="$(sha256sum "${vendor}/flexsearch.bundle.min.js")"
flexsearch_sha256="${flexsearch_sha256%% *}"

jq -n \
	--arg mermaid_version "${mermaid_version}" \
	--arg mermaid_source "${mermaid_url}" \
	--arg mermaid_sha256 "${mermaid_sha256}" \
	--arg flexsearch_version "${flexsearch_version}" \
	--arg flexsearch_source "${flexsearch_url}" \
	--arg flexsearch_sha256 "${flexsearch_sha256}" \
	'{
    "flexsearch.bundle.min.js": {sha256: $flexsearch_sha256, source: $flexsearch_source, version: $flexsearch_version},
    "mermaid.min.js": {sha256: $mermaid_sha256, source: $mermaid_source, version: $mermaid_version}
  }' >"${manifest}"

printf 'vendored mermaid %s and flexsearch %s\n' "${mermaid_version}" "${flexsearch_version}"
