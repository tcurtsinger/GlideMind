# GlideMind (`glm`) — Design Brief

A ServiceNow CLI whose differentiator is **context economy**: the fewest tokens per answered
question, for AI agents and humans alike. Prior art: [tehubersheezy/servicenow-cli](https://github.com/tehubersheezy/servicenow-cli),
[ewatch/snow-cli](https://github.com/ewatch/snow-cli) — both Rust, both "agent-friendly JSON."
GlideMind competes on tokens-per-answer, not endpoint breadth.

Decided 2026-07-21 via design interview. Each decision below is locked unless revisited deliberately.

---

## Decision log

| # | Decision | Choice |
|---|----------|--------|
| 1 | Differentiator | Deep context economy — measured, not vibes |
| 2 | Primary user (first 6 mo) | Author + their AI agents; no OSS tax yet, architect OSS-clean |
| 3 | Agent interface | CLI-first via shell; existing SN MCP fleet retires for daily work |
| 4 | v1 scope | Read + schema only; writes deferred behind a designed safety model |
| 5 | Language | Go |
| 6 | Auth | Named profiles + OS keyring + env-var overrides |
| 7 | Output default | Compact table (TTY) / TSV (piped); `--json` = JSONL |
| 8 | Default fields | Zero-config, auto-derived from instance metadata |
| 9 | Query syntax | Raw ServiceNow encoded query, no invented DSL |
| 10 | Bounds | limit 25, truncation with self-serve expansion hints, metadata on stderr |
| 11 | Grammar | Flat verb-first; binary named `glm` |
| 12 | Agent onboarding | `glm prime` generated from command registry + thin shipped skill |
| 13 | Pipe contract | `--format ids` + stdin batch get |
| 14 | Definition of done | 10-task benchmark; gate amended 2026-07-22 after measurement — correctness + dollar cost + per-question economy (see §12) |
| 15 | Pre-flight field validation | Validate field names in `--fields`/`-q` against schema cache; did-you-mean errors instead of SN's silent empty strings |
| 16 | `glm grep` in v1 | Multi-table code search across script fields (replaces `sn_dev search_scripts`) |
| 17 | `--since` time sugar | `--since 15m\|2h\|3d` compiles to created/updated time clause (log tailing) |
| 18 | `glm api` in v1 | `gh api`-style raw REST passthrough with profile auth + output formatting |
| 19 | SNFed/CAM governance layer | Out of scope permanently — those MCPs are defunct; no allowlist/sanitization layer needed |
| 20 | SmartWork strategy | glm stays **ServiceNow-generic, zero C1-specific code, forever**. Reads = generic queries; actions = `glm api` → the app's scripted REST endpoints (confirmed to exist); AI-orchestration verbs become Claude skills composing glm calls. SmartWork MCP retires as a fast-follow once those skills exist |

---

## 1. Positioning

Not another REST wrapper. The existing tools bound output size; GlideMind designs the entire
read path around an agent answering a question with minimal context spend: progressive
discovery (tables → schema → fields → records), display-value rendering, empty-field omission,
truncation that always says how to get the rest.

**Anti-goals (v1):** endpoint breadth, write operations, MCP server mode, OSS community support.

## 2. Architecture

- **Transport-agnostic core** (Go library) with the CLI as the first front-end.
- **Planned later phase** (explicit trigger: needing access from Claude Desktop / claude.ai /
  mobile): same core containerized in Azure/GCP behind a thin MCP shim — streamable HTTP via
  the official `modelcontextprotocol/go-sdk`, or a ~50-line external shim. **Zero MCP code in v1**,
  but no CLI-only assumptions in the core (no direct TTY access, no os.Exit, structured errors).
- Go because: single static binary, ~ms cold start (agents invoke hundreds of times/session),
  10–20 MB scratch/distroless images, trivial cross-compile, scale-to-zero friendly.

## 3. Command surface (v1)

Flat verb-first; hot path top-level, admin verbs grouped.

```
glm query <table> [-q <encoded>]... [--fields a,b,c] [--limit N] [--offset N] [--format ...]
glm get <table> <sys_id|number|name> [--fields ...] [--full]    # accepts '-' to read keys from stdin
glm count <table> [-q ...]
glm agg <table> --group-by <field> [--count|--sum f|--avg f|--min f|--max f] [-q ...]
glm schema <table> [--refresh]        # compact: field name, type, ref target, choice count
glm tables [pattern]                  # find tables by name/label
glm attach list <table> <key> | glm attach get <sys_id> [-o path]
glm grep <pattern> [--tables t1,t2] [--scope x_foo]   # code search across script fields
                                      # default tables: sys_script, sys_script_include,
                                      # sys_script_client, sys_ui_action, sys_ui_policy
                                      # output: table:name:field + matched lines only
glm api <METHOD> <path> [-f k=v]... [--body json]     # raw REST passthrough (gh api style)
                                      # profile-authed, same output formatting; non-GET
                                      # methods print the request and require --yes
glm whoami                            # identity, roles, instance sanity check
glm profile add|list|use|test|remove
glm prime                             # agent cheatsheet, ~400 tokens, generated from registry
```

- `query`/`count`/`agg`/`grep` accept `--since 15m|2h|3d` — compiles to the created/updated
  time clause so nobody hand-writes `javascript:gs.minutesAgoStart(15)`.
- **Pre-flight field validation:** field names in `--fields`/`-q` are checked against the schema
  cache before the request; unknown fields fail with did-you-mean. (The SN REST API silently
  returns empty strings for nonexistent fields — the #1 documented footgun in the MCP fleet.)

- `get` resolves human keys (INC0012345, a name) not just sys_ids, and shows **all non-empty
  fields** — empty-field omission alone drops a typical incident payload by more than half.
- Global flags: `--profile/-p`, `--json`, `--raw`, `--full`, `--format`, `--timeout`, `--verbose`.

## 4. Output contract

- **Default:** header + aligned columns on TTY; TSV when piped. `--json` = JSONL.
  Full set: `--format table|tsv|csv|json|jsonl|ids`.
- **Display values by default** ("In Progress", "John Smith"); `--raw` for machine values.
  The record's own `sys_id` is always available for chaining.
- **Rationale:** ~3x fewer tokens than JSON for tabular reads, and it's the most
  human-readable form — both audiences served by one default. Composability lives behind `--json`.

## 5. Default fields (zero-config law)

No curated lists, no per-table user config. Derived automatically from instance metadata:

1. Table's display field (dictionary),
2. semantic-role matches present on the table: number-ish, state-ish, priority, assigned_to/owner,
   active, `sys_updated_on`,
3. optionally informed by the instance's own `sys_ui_list` layout when present.

Identical behavior for OOB and `u_*` custom tables. `--fields` for explicit control
(dot-walking passes straight through to the API).

**Consequence:** a per-instance **schema cache** is a core subsystem — populated transparently on
first touch, local (`%LOCALAPPDATA%\glidemind\cache\<instance>\`), TTL ~7 days, `--refresh` to bust.

## 6. Query syntax

ServiceNow encoded query, verbatim: `-q/--query`, repeatable, joined with `^`
(`-q active=true -q priority=1` — avoids shell-quoting the caret). No sugar layer, no DSL.
Rationale: simple encoded queries already read as `field=value`; the platform UI copy-pastes
them ("Copy query"); LLMs know the syntax from training. A translation layer would add bugs
and docs while removing platform round-tripping.

## 7. Bounds & truncation

- `--limit 25` default; `--all` streams complete JSONL for exports.
- Table cells hard-truncated ~160 chars; JSON field values soft-capped 2,000 chars; `--full` lifts caps.
- **Data on stdout; summary + pagination on stderr** (`rows 1–25 of 1,847 · next: --offset 25`) —
  pipes stay clean, humans and agents still see it.
- Every truncation marker names the exact follow-up command
  (`…[+3.2KB — glm get incident INC0012345 --fields description --full]`).
  An agent must never dead-end on a cut-off value.

## 8. Errors, exit codes

- Errors teach: `unknown field 'severty' on incident — did you mean 'severity'? (see: glm schema incident)`.
  Structured, on stderr, with the corrective command when one exists.
- Deterministic exit codes (draft): 0 success · 1 usage/config · 2 auth · 3 API error ·
  4 network · 5 not found.

## 9. Auth & profiles

- Named profiles (instance URL, auth method, defaults) in a plain config file — **no secrets in it**.
- Secrets in the OS keyring (Windows Credential Manager / macOS Keychain / Secret Service).
- Everything overridable by env vars (`GLM_PROFILE`, `GLM_INSTANCE`, `GLM_TOKEN`,
  `GLM_CLIENT_ID`/`GLM_CLIENT_SECRET`…) so containers/CI work with zero code changes.
- v1 methods: basic auth + OAuth client-credentials. Interactive PKCE: later.

## 10. Agent onboarding

- `glm prime` emits a ~400-token cheatsheet (verbs, encoded-query reminders, output contract,
  pagination pattern, pipe idioms) **generated from the command registry** — cannot drift.
- Repo ships a thin Claude Code skill whose body is essentially "run `glm prime`, then work."
  Works identically for any agent runner.

## 11. Pipe contract

- `--format ids` → bare sys_ids, one per line.
- `glm get <table> -` → batch get from stdin, JSONL out.

```
glm query incident -q 'priority=1^opened_at>=javascript:gs.beginningOfThisWeek()' --format ids \
  | glm get incident - --fields description --full --json
```

jq remains the escape hatch for surgery; the common chain needs none.

## 12. Definition of done (v1)

1. Write down **10 real weekly tasks** (app-dev artifact reads, code search, schema verification,
   compliance traces/posture aggregates, SmartWork reads, log tailing) — kept in `BENCHMARK.md`.
2. An agent completes benchmark tasks via `glm` alone **with zero factual errors**
   (measured 2026-07-22: glm 2/2 clean; the MCP baseline produced wrong claims in 5 of 9 runs).
3. ~~Median session-token cost **≥5x cheaper** than the current SN MCP baseline on the same
   tasks.~~ **Amended 2026-07-22 after measurement** (full data and reasoning in
   BENCHMARK.md): the 5x session-token gate was calibrated against direct-API agents; inside
   an agent harness, every turn re-reads the shared session prefix, capping fresh-session
   ratios near 2x regardless of tool design — and a raw token sum overweights 0.1x-priced
   cache reads tenfold. Replacement gate, all measured: (a) ~half the dollar cost and
   wall-clock on heavy tasks, (b) order-of-magnitude fewer tokens per answered question in
   persistent sessions, (c) zero-error completions per item 2.
4. The `sn_*` dev-instance MCP server is actually disabled in daily sessions. (SNFed/CAM are
   already defunct — not part of the gate. SmartWork MCP scope: see §13.)

## 13. Workload map (from the real MCP fleet, 2026-07-21)

The four workloads glm must serve, per actual usage:

1. **App-dev artifact reads** — business rules, script includes, client scripts, UI policies,
   flows, update sets; `search_scripts` code search. → `query`/`get`/`grep` on `sys_script*` etc.
2. **Schema verification** — dictionary reads incl. inheritance chain, field existence checks
   during app development. → `schema`, pre-flight validation.
3. **Compliance/GRC** — NIST CSF v2.0 traces (framework → citations → controls), executive
   posture (control coverage, attestation gaps, riskiest open items). These MCP "summary" tools
   are just compositions (1 get + N counts/aggs) — the agent composes glm primitives instead.
4. **SmartWork** — reads are generic table queries (day one); domain ACTIONS go through the
   app's scripted REST endpoints via `glm api` (confirmed available; state models and side
   effects stay server-side where they belong). AI-orchestration verbs (scaffold/enrich) become
   Claude skills that compose glm calls. **End state: no SmartWork MCP** — its retirement is a
   fast-follow after v1, gated on those skills existing, not part of the v1 gate itself.

## Implementation defaults (decided by fiat — cheap to revisit)

- HTTP: 30s default timeout (`--timeout`), retry with backoff+jitter on 429/503, honor Retry-After.
- Reads use the Table API with `sysparm_display_value` chosen per format; `sysparm_exclude_reference_link=true`.
- Config: `%APPDATA%\glidemind\config.toml` (XDG paths elsewhere).
- Repo layout: `cmd/glm` (CLI) + `internal/core` (transport-agnostic engine) + `internal/snow` (API client).

## Deferred (explicitly not designed yet)

- Write verbs + safety model (per-profile write enablement, `--dry-run`, confirmations, audit trail)
- Containerized deployment + MCP facade (trigger: Claude Desktop/web need)
- Interactive OAuth (PKCE) · attachment upload · import sets · background scripts · update sets
- Watch/streaming · cross-instance federated queries · distribution (scoop/brew/goreleaser) · OSS release
