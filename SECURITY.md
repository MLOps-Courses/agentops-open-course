# Security Policy

## Which versions receive security fixes?

This course is pre-1.0. Security fixes are applied to the latest commit on `main`; older snapshots and forks are not maintained by this repository.

## How do I report a vulnerability?

Do not open a public issue for a suspected vulnerability, leaked credential, prompt-injection bypass with real impact, or supply-chain compromise.

Email **agentops-open-course@fmind.dev** with:

- The affected file, component, or release.
- Reproduction steps or a minimal proof of concept.
- The realistic impact and required preconditions.
- Any suggested mitigation.

Remove API keys, access tokens, personal data, and customer data from the report. The maintainers aim to acknowledge a complete report within three business days and will coordinate disclosure after a fix is available.

## What is in scope?

- The reference agent and its local data boundaries.
- Guardrails, approval handling, and audit behavior.
- Container and Kubernetes configuration shipped by this repository.
- Documentation that would cause a learner to expose credentials or deploy an insecure default.
- Repository automation and dependency supply chain.

The behavior of third-party model providers, cloud services, and upstream projects should normally be reported to their respective maintainers. A configuration problem caused by this repository remains in scope.

## What should I do if I committed a secret?

1. Revoke or rotate it immediately at the provider.
1. Remove it from the working tree and history.
1. Run `mise run scan` against the full Git history.
1. Report the exposure privately if the repository or its users may be affected.

Deleting a secret from the latest commit does not revoke it and does not remove it from Git history.
