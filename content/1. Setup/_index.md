---
title: "1. Setup"
description: Take a clean clone to a green offline suite and a local model, installing each heavier dependency only on the page that actually needs it.
slug: "1-setup"
aliases:
  - "/1. Setup/index.html"
---

{{% admonition abstract "In one glance" %}}

- **You will:** See how this chapter is staged, learn the one command that reads your machine, and know which pages you can skip today.
- **You need:** Git, a Unix-like shell, and permission to install tools into your own user account.
- **Time:** about 6 minutes, orientation. {{% /admonition %}}

## How this chapter is staged, and what each stage buys

Chapter 1 builds a **pinned, checkable development environment**: one table of tool versions, and one command per **doctor profile**. That profile is a named tier of prerequisites the doctor script compares against your machine. Pinning makes verdicts portable, because the linters, compilers, and test suites that decide them agree across machines only when they are the same programs.

Heavier prerequisites cost download time, disk, and eventually money, so the chapter is staged: each stage buys something specific. The offline stage — 1.0. System, 1.1. Go, and 1.5. Workspace — costs a download and buys a compiler, a linter, and a test suite that decide most of the agent's behavior. The model stage, 1.4. Providers, costs 2.5 GB of weights and buys an agent that answers questions on your own hardware. The container, Kubernetes, and cloud stages cost progressively more and buy nothing you need before Chapter 5, so the required path installs none of them and asks for no account or API key.

You cannot run this yet — mise arrives with [1.0. System]({{< relref "/1. Setup/1.0. System.md" >}}) — but here is what it reports there once the base tier is ready:

```bash
mise run doctor
```

```text
[doctor] $ ./scripts/doctor.sh base
base       ready
env        optional .env is absent
```

Two lines of verdict follow the command mise echoes, and neither is a blessing. The first says the pinned tools this repository shells out to are all on your `PATH` — presence, not proof that anything works. The second says no `.env` file exists, which is not a warning: the deterministic checks never read that file even when it exists. Their verdicts describe the committed source rather than your shell, and a check that read your shell could go green on a wrong commit.

**By the end of [1.4. Providers]({{< relref "/1. Setup/1.4. Providers.md" >}}) a language model will be answering on your own hardware — and by the end of [1.1. Go]({{< relref "/1. Setup/1.1. Go.md" >}}) you will have watched a suite of 1,828 tests report 1,827 passed and one skipped, without a model anywhere near it.**

## Which pages reach your first turn, and which wait until you change something

Two pages stand between a clean clone and an agent that answers — about fifty minutes, most of it a download. The other two matter the first time you edit the repository rather than read it, which is why the express route in [0.0. Course]({{< relref "/0. Overview/0.0. Course.md" >}}) skips them and this one does not pretend otherwise.

Before your first turn:

- **[1.0. System]({{< relref "/1. Setup/1.0. System.md" >}})** _(hands-on)_: install the pinned toolchain and take a clean clone to a green offline suite.
- **[1.4. Providers]({{< relref "/1. Setup/1.4. Providers.md" >}})** _(hands-on)_: Ollama, the Qwen3 pull, the model doctor, and the deadline that trips first-hour learners.

Before you change anything — another forty minutes, and worth it the moment a check fails and you need to know whose fault it is:

- **[1.1. Go]({{< relref "/1. Setup/1.1. Go.md" >}})** _(hands-on)_: the three Go modules, the dependencies the agent actually ships, and the coverage floor.
- **[1.5. Workspace]({{< relref "/1. Setup/1.5. Workspace.md" >}})** _(hands-on)_: what is source, what is disposable, and which check runs at which moment.

Two more sit in this chapter but belong to later ones. **[1.2. Container Engine]({{< relref "/1. Setup/1.2. Container Engine.md" >}})** _(hands-on)_ prepares a Docker-compatible engine and is reached from [5.1. Gateway Setup]({{< relref "/5. Gateway/5.1. Gateway Setup.md" >}}); **[1.3. Kubernetes]({{< relref "/1. Setup/1.3. Kubernetes.md" >}})** _(reference)_ prepares the local platform tier and is reached from [6.1. Containers]({{< relref "/6. Platform/6.1. Containers.md" >}}).

## A green doctor profile implies nothing about the next tier

Profiles are independent diagnostics. A green base profile says nothing about whether a model is serving, and a green model profile says nothing about whether Docker is running.

| What you are about to do                      | Profile that proves you are ready                 |
| --------------------------------------------- | ------------------------------------------------- |
| Chapters 0 and 1, plus every offline check    | `mise run doctor`                                 |
| Any model-backed page in Chapters 2 through 4 | `mise run doctor:model`                           |
| The Chapter 5 host gateway                    | `mise run doctor:gateway` plus the model profile  |
| The Chapter 6 local platform                  | `mise run doctor:platform`                        |
| The optional GKE laboratory                   | `mise run doctor:gcp` and explicit cloud approval |

Setup itself never starts a model, a gateway, a cluster, or a cloud resource; each has an owning page later that can also state its cost and how to tear it down.

## What this chapter proved

The chapter is still ahead of you; this page has settled four things:

- `mise run doctor` reports the base profile ready on your machine, or names the tool that is missing and the command that installs it.
- You can say which two pages reach your first turn, which two follow, and which two the navigation returns you to later.
- You can name the profile that proves each later chapter's prerequisites before you open that chapter.
- The required path asks you for no credential at all — the optional Gemini and cloud branches on [1.4. Providers]({{< relref "/1. Setup/1.4. Providers.md" >}}) are the only places one appears.

An hour from now the phrase "it works on my machine" will have a precise, checkable meaning here.

Continue to [1.0. System]({{< relref "/1. Setup/1.0. System.md" >}}) and install the toolchain.
