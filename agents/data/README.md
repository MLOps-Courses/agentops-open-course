# Ops Copilot Dataset

The **immutable seed** for the AgentOps Open Course reference agent (the "Ops Copilot"). Data access and deterministic tests run offline after installation; an interactive agent still needs local Ollama or a configured model provider. Every action is mock and local: nothing here touches real infrastructure.

## Contents

- `sql/schema.sql` — the SQLite schema (`services`, `incidents`, `audit_log`).
- `sql/seed.sql` — deterministic seed rows (fixed ids and timestamps → reproducible evals): 8 services and 10 incidents, including a cascading failure chain (INC-007 cache eviction → INC-008 database overload → INC-009 checkout latency) and an ambiguous open incident (INC-010) for retrieval evals.
- `incidents.db` — the generated database, committed so the agent runs out of the box.
- `runbooks/*.md` — the knowledge base for memory / RAG (Ch. 3.4); each `incidents.runbook` value links to `runbooks/<slug>.md`.
- `logs/*.log` — sample service logs read by the `search_service_logs` tool (checkout, inventory, cache, database), with interleaved INFO noise and red herrings for realistic triage.
- `skills/*/SKILL.md` — allowlisted, progressively disclosed operating instructions.

## Rebuild the database

`incidents.db` is generated from the SQL; regenerate it whenever the SQL changes:

```bash
mise run build      # rm -f incidents.db && sqlite3 incidents.db < sql/{schema,seed}.sql
mise run check      # byte-for-byte rebuild + referential integrity (services ↔ incidents ↔ runbooks ↔ logs)
```

## How the agent locates this data

The agent resolves immutable inputs from `AGENT_DATA_DIR`, falling back to this folder. Its writable SQLite copy lives under `AGENT_STATE_DIR` (default: `agents/python/.state`), so local actions never mutate the committed seed. Run `cd agents/python && mise run data:reset` to restore deterministic state. The container mounts a separate writable state volume and keeps this bundled directory read-only.
