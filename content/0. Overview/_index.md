---
title: "0. Overview"
description: "Orient before you build: see what the agent is grounded in, decide when an agent is justified, learn what a claim in this course is worth, and pick your path."
slug: "0-overview"
aliases:
  - "/0. Overview/index.html"
---

{{% admonition abstract "In one glance" %}}

- **You will:** Decide whether an agent is the right tool for a job, learn what this course's green checkmarks are worth, and pick your model path.
- **You need:** Nothing installed. Chapter 0 is read-only.
- **Time:** about 4 minutes, orientation. {{% /admonition %}}

## Why the course decides three things before it installs anything

Chapter 0 installs nothing. Three decisions are far cheaper to make now than after a cluster is running: whether an agent is the right tool for your problem at all, what a passing check in this course actually proves, and which model path you will follow. Get the first one wrong and you spend eight chapters giving autonomy to something a plain function should have done. Get the second wrong and you ship on a number that never meant what you assumed.

The course teaches those decisions against one worked example, so the mechanics stay concrete rather than abstract: an on-call assistant that reads a fictional platform's incidents, service logs, and runbooks, and proposes remediation a human has to approve. The whole domain is committed to this repository — ten incidents, four service logs, seven runbooks, two Agent Skills — and this is the part of it that is currently open:

```bash
sqlite3 -header -column agents/data/incidents.db \
  "select id, service, severity, status from incidents where status = 'open';"
```

```text
  id       service    severity  status
-------  -----------  --------  ------
INC-002  inventory    SEV1      open
INC-005  search       SEV3      open
INC-010  api-gateway  SEV3      open
```

That table is why this domain was chosen. No language model can have memorised `INC-002`, so every claim an agent makes about it either came from reading that file or came from nowhere — which turns the example into a measuring device you can point at. The domain is not the subject. Nobody here is learning incident management, and [8.7. Capstone]({{< relref "/8. Community/8.7. Capstone.md" >}}) replaces the fiction with a problem of your own.

You cannot run that query yet, and that is deliberate: `sqlite3` arrives with the pinned toolchain in [1.0. System]({{< relref "/1. Setup/1.0. System.md" >}}). The same holds for every command block in this chapter — each is a transcript of a real run, shown so a claim has evidence beside it, and none of them is asking you to type anything before Chapter 1.

## Which three pages to read now, and which six to bookmark

Chapter 0 is the only part of the course you cannot run. It earns its place by being short and by ending in decisions rather than exercises. The hands-on work starts the moment you install. Read these three in order:

- **[0.0. Course]({{< relref "/0. Overview/0.0. Course.md" >}})** _(orientation · ~8 min)_: what you will have built, which path to take, and what it costs.
- **[0.1. Agents]({{< relref "/0. Overview/0.1. Agents.md" >}})** _(concept · ~12 min)_: what an agent actually is, and when a plain function beats one.
- **[0.2. Evidence]({{< relref "/0. Overview/0.2. Evidence.md" >}})** _(concept · ~8 min)_: what a gate proves, what an observation suggests, and why the course never blurs them.

The other six are reference — open them when a chapter sends you. [0.3. AgentOps]({{< relref "/0. Overview/0.3. AgentOps.md" >}}) maps the lifecycle to the chapters, [0.4. Ecosystem]({{< relref "/0. Overview/0.4. Ecosystem.md" >}}) says who owns which boundary and port, [0.5. Provider Options]({{< relref "/0. Overview/0.5. Provider Options.md" >}}) settles the model choice, [0.6. Resources]({{< relref "/0. Overview/0.6. Resources.md" >}}) says which source wins when two disagree, [0.7. Troubleshooting]({{< relref "/0. Overview/0.7. Troubleshooting.md" >}}) fixes a failing checkpoint, and [0.8. Glossary]({{< relref "/0. Overview/0.8. Glossary.md" >}}) defines every term in one line — keep that one open in a second tab.

## What this chapter proved

- You know which three decisions Chapter 0 exists to settle, and why making them later is more expensive.
- You know the worked example is a committed SQLite seed with ten incidents in it, three of them open, chosen because no model could have memorised its contents.
- You know which three pages to read now and which six to bookmark.
- You know that Chapter 0 installs nothing: [1.0. System]({{< relref "/1. Setup/1.0. System.md" >}}) owns the clone and the toolchain.

You now know what this repository contains, what the course will make you responsible for, and which page to open first. That last one is the only decision this page was ever asking for.

Continue to [0.0. Course]({{< relref "/0. Overview/0.0. Course.md" >}}), which shows you the first thing the agent does with a question it cannot answer from memory.
