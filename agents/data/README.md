# Ops Copilot Dataset

The **shared, single source of truth** for the AgentOps Open Course reference agent (the "Ops Copilot"). Both tracks — [`../python`](../python) and [`../go`](../go) — read this directory, so the agent behaves identically and runs **fully offline**. Every action is mock and local: nothing here touches real infrastructure.

## Contents

- `sql/schema.sql` — the SQLite schema (`services`, `incidents`, `audit_log`).
- `sql/seed.sql` — deterministic seed rows (fixed ids and timestamps → reproducible evals).
- `incidents.db` — the generated database, committed so the agent runs out of the box.
- `runbooks/*.md` — the knowledge base for memory / RAG (Ch. 3.4); each `incidents.runbook` value links to `runbooks/<slug>.md`.
- `logs/*.log` — sample service logs used in diagnosis examples.

## Rebuild the database

`incidents.db` is generated from the SQL; regenerate it whenever the SQL changes:

```bash
mise run build      # rm -f incidents.db && sqlite3 incidents.db < sql/{schema,seed}.sql
mise run check      # sanity-check row counts
```

## How the tracks locate this data

Each track resolves the data directory from the `AGENT_DATA_DIR` environment variable, falling back to this folder (resolved relative to the source). When the agent is containerized (Ch. 6), the image copies this directory in and sets `AGENT_DATA_DIR`.
