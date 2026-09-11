---
title: "4. Quality"
description: "Prove the agent behaves under adversarial pressure: types, linting, tests, metrics, evaluations, guardrails, and security."
slug: "4-quality"
aliases:
  - "/4. Quality/index.html"
---

{{% admonition abstract "In one glance" %}}

- **You will:** See which failure each of the chapter's seven layers catches, and run the three offline commands that cover all of them.
- **You need:** `mise run install` done. No model, key, container, or network.
- **Time:** about 6 minutes, orientation. {{% /admonition %}}

## Three failure modes no single gate catches

**Agent quality** is several unrelated properties, not one. Values crossing a boundary hold their shape, the code means what it says, behavior survives hostile input, and a reported number means what you take it to mean. No compiler, test suite, or dashboard proves more than one.

Chapters 2 and 3 gave the agent a conversation and capabilities. Neither establishes that it behaves when its inputs are hostile, nor that a green result means what it appears to mean. So this chapter is seven layers ordered as a cost curve, each catching failures the cheaper layers cannot see, plus three offline commands covering all seven.

Three examples make that concrete. A value that compiles: a `string` holding `INC-2; DROP TABLE incidents`. Neither the compiler nor a test that never thought to try it objects. A value that lies: a confident paragraph with nothing underneath it, indistinguishable, from the outside, from the grounded one in [2.1. First Agent]({{< relref "/2. Agents/2.1. First Agent.md" >}}). A run green for the wrong reason: a suite that passes because its interesting case was never written, or a pass rate that clears its floor while the safety case fails.

## Seven layers, and the failure each one catches

The first three layers are free, offline, and instant. Only 4.3 and 4.4 buy answers a running model alone gives; the last two return offline, apart from an optional gateway run and the scanners' network.

- **[4.0. Type Safety]({{< relref "/4. Quality/4.0. Type Safety.md" >}})** _(concept · offline)_: a hostile tool argument dies at the boundary that owns it, instead of reaching the database.
- **[4.1. Linting]({{< relref "/4. Quality/4.1. Linting.md" >}})** _(hands-on · offline)_: an ignored error in a transaction — a bug that compiles perfectly — is caught by a machine.
- **[4.2. Testing]({{< relref "/4. Quality/4.2. Testing.md" >}})** _(hands-on · offline)_: the model is replaced and everything else stays real, so most of the agent is decided in seconds.
- **[4.3. Metrics]({{< relref "/4. Quality/4.3. Metrics.md" >}})** _(reference · needs a model for the live rows)_: a scorecard where every number names its command, candidate, and proof class.
- **[4.4. Evaluations]({{< relref "/4. Quality/4.4. Evaluations.md" >}})** _(hands-on · offline validation, model-backed runs)_: black-box turns over the wire, scored by structure first and by a judge only for the residue.
- **[4.5. Guardrails]({{< relref "/4. Quality/4.5. Guardrails.md" >}})** _(hands-on · offline, except the optional gateway run)_: injected text is defused, personal data is masked twice, and no write lands without confirmation and one transaction.
- **[4.6. Security]({{< relref "/4. Quality/4.6. Security.md" >}})** _(hands-on · offline; scanners may use the network)_: authority the agent never holds cannot be misused, and a suite proves nobody quietly added it back.

Every **Your turn** here is optional and self-contained: no later page's prerequisites name what one leaves behind, so read the page, take the code, and skip the exercise if you came for the argument. Three break something on purpose and restore it — [4.1. Linting]({{< relref "/4. Quality/4.1. Linting.md" >}}), [4.5. Guardrails]({{< relref "/4. Quality/4.5. Guardrails.md#your-turn-does-a-tool-name-prove-trusted-provenance" >}}), [4.6. Security]({{< relref "/4. Quality/4.6. Security.md" >}}) — and three leave a case you keep: [4.2. Testing]({{< relref "/4. Quality/4.2. Testing.md" >}}), [4.4. Evaluations]({{< relref "/4. Quality/4.4. Evaluations.md#your-turn-how-do-you-add-one-adversarial-case-and-deterministic-validator" >}}), and 4.5 again.

One distinction runs through all seven pages: a **gate** is deterministic, same input, same verdict, every time; an **observation** measures something that varies, such as a pass rate. [0.2. Evidence]({{< relref "/0. Overview/0.2. Evidence.md" >}}) owns it; each page links there rather than re-deriving it.

## Three offline commands cover all seven layers

Three commands, run from the repository root, none needing a model, provider key, or network:

```bash
mise run test
mise run redteam
mise run eval:validate
```

`mise run test` is the widest: it fans out to all three Go modules — the agent in `agents/go`, the harness in `evals`, the maintainer tooling in `tools` — and two of them end by enforcing a coverage floor. `mise run redteam` runs the deterministic adversarial regressions in the agent's `policy` package; `mise run eval:validate` proves the committed evalsets against the seed, the committed incident dataset.

```text
[test:tools] DONE 392 tests in 21.897s
[test:evals] DONE 425 tests in 16.064s
[test:evals] evals meets the 80% per-package coverage floor
[test:go] DONE 1828 tests, 1 skipped in 18.886s
[test:go] agents/go meets the 80% per-package coverage floor
Finished in 31.85s
```

Six lines from that run, in the order they arrived. The three suites run concurrently, so the full log interleaves them and prints a coverage line per package; `tools` deliberately sits outside the floor.

## What this chapter proved

- Just over twenty-six hundred tests decide the agent, the evaluator, and the repository tooling under the race detector in well under a minute, with no service to start and nothing spending a token.
- `mise run redteam` settles the adversarial policy cases and `mise run eval:validate` proves the evalsets against the seed, both offline.
- By the end you can take any green result in this chapter and say whether it is a gate or an observation — and name the command behind it.

The guardrails were already there; the agent refuses hostile input either way. These seven pages let you prove it in a second, before the agent sits behind a gateway any local process can reach.

Continue to [4.0. Type Safety]({{< relref "/4. Quality/4.0. Type Safety.md" >}}); the first three pages need no model and no account.
