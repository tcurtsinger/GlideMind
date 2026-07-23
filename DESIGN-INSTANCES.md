# GlideMind Multi-Instance Model — Design Addendum

**Status: LOCKED (2026-07-23).** Companion to [DESIGN-WRITES.md](DESIGN-WRITES.md). Folds into
DESIGN.md when the instance-ergonomics PR lands.

## The problem this solves

With an MCP fleet, instance selection is *implicit in tool choice*: each server binds to one
instance, the binding lives in the tool name, and nothing in the call says where it's going. AI
agents pick by inference, pick wrong constantly, and identical-looking clone data (dev instances
cloned from production share sys_ids) confirms the wrong guess instead of exposing it. glm inverts
this — one tool, instance as an explicit argument — and this addendum closes the two remaining
leaks: the silent default and invisible execution.

## Decision log

| # | Decision | Choice |
|---|----------|--------|
| I1 | Selection | 1 profile → implicit; 2+ profiles → `-p` required (exit 1 + profile list if omitted) |
| I2 | Default opt-out | `glm profile use <name>` deliberately restores a default; stored, like `writable` |
| I3 | Stamping | With 2+ profiles, every command stamps `instance: <profile> (<host>)` on stderr |
| I4 | Agent onboarding | `glm prime` lists configured profiles (+ default, + writable flags) up front |
| I5 | Cross-instance diff | `glm diff` — record diff and schema diff between two instances |

## I1/I2 — Derived explicitness

Zero-config principle applied to safety: the requirement derives from the profile count.

- **One profile**: it is the instance; requiring `-p` would be ceremony. Implicit.
- **Two or more**: ambiguity exists, so glm refuses to guess. Omitting `-p` exits 1 with
  `multiple profiles configured (dev, smartwork, qa): pass -p <name>` — an agent self-heals from
  that in one turn, and a silent wrong-instance call becomes impossible.
- **`glm profile use <name>`** is the human escape hatch: a deliberate, stored choice to
  restore implicit selection (clear with `glm profile use --clear`). Even with a default set,
  stamping (I3) still applies — convenience never removes the evidence trail.
- **Legacy migration**: configs written before this change carry a default auto-set by
  `profile add`. Adding the second profile clears it (with a message naming the restore
  command) — a default set while only one profile existed never had an observable effect, so
  nothing deliberate is lost. Defaults chosen in an already-multi-profile world stay.

## I3 — Instance stamping

With 2+ profiles configured, every command writes one line to stderr:

```
instance: dev (ven07100.service-now.com)
```

Rides the existing stdout=data / stderr=metadata contract — pipes stay clean, transcripts prove
where every result came from, and a misattributed answer becomes visible instead of latent. With
one profile the line is omitted: no tokens spent on a confusion that cannot occur.

## I4 — Prime teaches instance awareness

`glm prime` opens with the profile table: name, host, default marker, writable flag. An agent's
first command of the session therefore establishes which instances exist and that `-p` is
required — the skill reinforces it, but the binary is the source of truth.

## I5 — `glm diff`

The workflow that motivates multi-instance support is *compare*: "works in SmartWork, broken in
dev — what's different?" MCP fleets cannot answer this at all; glm makes it one command.

- **Record diff**: `glm diff <table> <key> -p A -p B` — fetches the record from both instances,
  prints **only the differing fields** (field, A value, B value). `--fields` narrows; `--full`
  shows untruncated values; `--json` for machine output. Stored values, consistent with query
  semantics.
- **Schema diff**: `glm diff <table> -p A -p B` (no key) — compares the table's schema: fields
  present in one instance but not the other, and type/reference mismatches. This is the
  "missing column in dev" troubleshooting answer.
- **Key resolution**: number or sys_id via `get`'s resolver. Number is the reliable cross-instance
  key; sys_id equality only holds between clones (and clone drift makes even that untrustworthy —
  resolve per-instance, never assume).
- **Exactly two `-p` flags** required; first is left/base, second is right.
- **Missing on one side** is a diff *result* (`record not found in dev`), exit 0. Missing on both
  sides → exit 5. Differences never affect the exit code — they are data, not errors
  (a Unix-`diff`-style exit code would collide with the 0–5 contract).
- Read-only; no interaction with the write gates.

## Interplay with writes

- `writable` is per-profile (W1). SmartWork is planned to be write-enabled too (user decision,
  2026-07-23) — per W9, OAuth-as-user should land first so live-instance writes stamp the real
  user, not `svc.glm`. Until then its profile stays read-only.
- The write preview names profile + host + identity (W7), so instance and actor are visible at
  the moment of every write.
- I1 applies to writes identically: with 2+ profiles a write with no `-p` never reaches the
  confirmation stage.

## Deferred

- Fan-out reads (`-p a,b,c` running one query against N instances) — no motivating workflow yet.
- Env-var profile pinning (`GLM_PROFILE`) — invisible session state is the failure mode this
  addendum exists to kill; revisit only with a concrete CI need, same reasoning as `GLM_WRITABLE`.
- Cross-instance *record sync* ("copy this record to dev") — that's a write feature with its own
  safety questions; not in scope.
