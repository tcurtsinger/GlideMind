# GlideMind

`glm` — a context-economical ServiceNow CLI for AI agents and humans.

**Status: pre-alpha.** Nothing installable yet. The full design rationale lives in
[DESIGN.md](DESIGN.md); the success scoreboard lives in [BENCHMARK.md](BENCHMARK.md).

## Why another ServiceNow CLI

Existing tools wrap the REST API. GlideMind optimizes a different metric: **tokens per answered
question**. Every default is chosen so an agent (or a human) spends the least possible context
getting a correct answer:

- Compact table output by default (~3x cheaper than JSON for tabular reads); `--json` when composing.
- Zero-config default fields, auto-derived from instance metadata — OOB and custom tables alike.
- Native ServiceNow encoded queries — no invented DSL.
- Pre-flight field validation against a local schema cache: did-you-mean errors instead of the
  REST API's silent empty strings.
- Bounded output with self-serve pagination hints; truncated values carry a marker showing how
  to lift the cap (`--full`), and `grep`'s remainder marker names the exact `glm get` to run.
- `glm grep` — code search across script tables. `glm api` — authenticated raw REST passthrough.

## Principles

1. **Context economy** — measured against a 10-task benchmark, not vibes.
2. **Composability** — clean stdout, metadata on stderr, `--format ids` + stdin batch get.
3. **ServiceNow-generic forever** — no app-specific or vendor-specific code; app-layer workflows
   go through `glm api`.
