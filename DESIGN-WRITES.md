# GlideMind Write Safety Model — Design Addendum

**Status: LOCKED (2026-07-23).** All four open questions resolved (see Resolutions at the end).
Resolves the "writes deferred behind a designed safety model" item in [DESIGN.md](DESIGN.md)
§12/Deferred. DESIGN.md gains a pointer to this addendum when the safety-core PR lands.

This addendum designs *how* glm writes, not whether. The transport already exists and is
audit-hardened: `glm api` with a non-GET method works today, the client refuses to follow
redirects on a write (no body-replay), a non-GET request goes on the wire exactly once, and the
`--yes` preview shows precisely what is sent. What is missing is the *policy layer* — the gates,
confirmations, validation, and traceability that make writes safe to hand an agent.

## Constraints this must honor (from the locked design)

- **ServiceNow-generic forever.** Writes are generic table CRUD + the `api` passthrough. No
  app-specific write logic — domain actions (e.g. SmartWork transitions) still go through
  `glm api` to scripted REST endpoints (Decision 20).
- **Zero-config.** No per-table write rules or curated lists. Behavior auto-derives; overrides
  are flags/profile settings (Decision 8).
- **Read-only by default.** A profile writes nothing until explicitly enabled. Reads never change.
- **Attribution.** Writes stamp `sys_updated_by`. On shared/prod instances that should be *you*
  (OAuth-as-user), not the `svc.glm` service account — see W9.

## Decision log (proposed)

| # | Decision | Choice |
|---|----------|--------|
| W1 | Enablement | Two gates: profile `writable=true` (default false) AND per-command confirmation |
| W2 | Surface | `api` passthrough stays the escape hatch; add generic verbs `create`/`update`/`delete` |
| W3 | Validation | Strict pre-flight field check on writes (unknown field = hard error); raw stored values |
| W4 | Preview | Every write previews method/URL/identity/payload; `update` shows a field-level diff |
| W5 | Confirmation | `--yes` for non-interactive; interactive TTY prompts, typed confirm for destructive |
| W6 | Audit | Local append-only JSONL of what glm wrote (field names + identity + result, not values) |
| W7 | Identity | The write preview names the authenticated user, so you never write as the wrong one |
| W8 | Exit codes | Reuse 0–5: gate/validation = 1, not-found = 5, API error = 3 |
| W9 | OAuth interplay | Writes allowed on basic auth (dev); OAuth-as-user recommended before real-instance writes |
| W10 | Still deferred | Bulk/import, attachment upload, background scripts, transactions/rollback, choice-label resolution |

---

## W1 — Two-gate enablement

A write requires **both**:

1. **A writable profile.** `writable = true` in the profile (default absent/false). Set at
   creation (`glm profile add … --writable`) or later (`glm profile write-enable <name>`). A
   read-only profile refuses every write with a one-line error naming how to enable it. There is
   deliberately **no `GLM_WRITABLE` env override** — flipping write access by environment is too
   easy to do by accident in the wrong shell; writability is a deliberate, stored profile property.
   Consequence: the synthetic `GLM_INSTANCE` env profile is **always read-only** — env-only
   (container/CI) write access would be exactly the invisible-state gate this decision rejects.
2. **Per-command confirmation** (W5). Writable is necessary, not sufficient.

Rationale: the dangerous mistake is writing to prod thinking you're on dev. A stored per-profile
flag makes "can this profile write at all?" an explicit, auditable setup step, and the two gates
mean neither a mis-set profile nor a stray `--yes` alone can cause a write.

## W2 — Command surface

Layered, so the generic escape hatch and the ergonomic common case coexist:

- **`glm api <METHOD> <path>`** — unchanged. The raw passthrough for anything the verbs don't
  cover (scripted endpoints, unusual APIs). Already gated by `--yes`.
- **Generic table verbs** (new), thin wrappers over the same transport:
  - `glm update <table> <key> -f field=value …`
  - `glm create <table> -f field=value …`
  - `glm delete <table> <key>`

The verbs add what `api` can't: field validation (W3), key resolution (`update incident INC0012345`
reuses `get`'s resolver), a readable diff/preview (W4), and consistent exit codes. They are
**generic table CRUD only** — no app semantics.

## W3 — Strict field validation on writes

Writes validate `-f` field names against the schema (reusing `validateFields`, including the
self-heal refetch) and **hard-fail on an unknown field before sending**. This is stricter than
reads on purpose: ServiceNow *silently ignores* unknown fields on a write, so a typo'd field name
is silent data loss — the single worst write footgun. Values are raw stored values (`-f state=2`,
not `-f state="In Progress"`); choice-label resolution is deferred (W10).

## W4 — Preview and diff

Every write prints, before sending: the method, target URL, table + key, the fields being set,
and the authenticated identity (W7). For **`update`**, glm does a read-before-write (one GET) and
shows a field-level diff — `state: 1 → 2` — so you approve the actual change, not just an intent.
The diff is the single best safety feature; it is on by default interactively (`--no-diff` skips
it), and with `--yes` the extra GET is skipped unless `--diff` is passed (keeps scripted writes
cheap). `--dry-run` prints the full preview and exits 0 without sending.

## W5 — Confirmation

- **`--yes`** satisfies confirmation non-interactively (agents, pipes) — same contract as `api`
  today.
- **Interactive TTY**: `create`/`update` take a simple `y/N`; **`delete`** requires typing the
  record's number/sys_id to confirm. That is the whole v1 rule — a generic tool cannot know
  which *updates* are high-impact without per-table configuration, which would violate
  zero-config; the update diff (W4) is the safeguard there instead.
- Confirmation is orthogonal to the profile gate (W1): both always apply.

## W6 — Local audit log

An append-only JSONL log of glm's own writes at `%LOCALAPPDATA%\glidemind\audit.jsonl`
(`GLM_AUDIT_LOG` overrides; `--no-audit` skips one call). Each line records timestamp, instance,
authenticated user, verb, method, table, sys_id, **field names** (not values — avoids hoarding
sensitive data locally), and result/exit. This is local traceability of what glm did; it
complements, and does not replace, ServiceNow's server-side `sys_audit`/history.

## W7 — Identity in the preview

The preview line names who glm is authenticating as (`writing to incident/INC0012345 as
tcurtsinger @ ven07100`). glm already knows this (`whoami`). It makes the attribution question
(W9) visible at the moment of the write, so a write never lands under an unexpected identity.

## W8 — Exit codes

Reuse the 0–5 contract: profile-not-writable or missing confirmation → 1 (usage); unknown field →
1; not-found key on `update`/`delete` → 5; API error on the write → 3; success → 0.

## W9 — OAuth interplay (attribution)

Writes are **permitted on basic-auth profiles** — for the dev instance where attribution doesn't
matter, that's the fast path. But on shared/production instances, a write via `svc.glm` stamps the
service account, not you. The recommendation (not a hard block — that would be paternalistic) is:
enable **OAuth-as-user before writing on any instance where `sys_updated_by` matters**, at which
point the service account retires to CI/automation duty. W7's identity-in-preview makes the
tradeoff visible rather than enforcing it. This is the same ordering flagged when the service
account was created as a 2FA workaround.

## W10 — Still deferred after this

Single-record generic CRUD + `api` passthrough is the whole v1 write scope. Explicitly still out:
bulk/batch writes, import sets, **attachment upload**, background scripts, multi-record
transactions/rollback, and choice-label→value resolution.

---

## Resolutions (locked 2026-07-23)

1. **Verbs.** Ship `create`/`update`/`delete` — the field validation and update-diff are the
   safety value `api` cannot provide. `api` remains the escape hatch.
2. **Update diff default-on.** Interactive `update` always reads-before-write and shows the
   field-level diff (`--no-diff` skips); `--yes` skips the extra GET unless `--diff` is passed.
3. **Audit log: field names only.** No values at rest locally; ServiceNow's server-side
   `sys_audit`/history keeps values.
4. **Enablement: profile flag only.** No `GLM_WRITABLE` env override — writability is a
   deliberate, stored profile property.

## Proposed rollout (once locked)

1. **Safety core** (1 PR): profile `writable` gate + `glm api` respecting it + the audit log +
   identity-in-preview. Makes the *existing* `api` write path safe and traceable. ~1–2 days.
2. **Verbs** (1–2 PRs): `update` (with diff + validation), then `create`/`delete`. ~3–4 days.
3. **(Separately) OAuth PKCE** — its own track (~1 week); land before real-instance write use.
