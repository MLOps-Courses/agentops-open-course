# Accepted Skaffold runbook

The command runs from `infra/`, so the manifest is named relative to that directory and the profile is spelled out in full.

```bash
cd infra
skaffold delete --filename skaffold.yaml --profile local
```
