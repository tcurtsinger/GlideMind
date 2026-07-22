# GlideMind v1 Benchmark

The scoreboard for DESIGN.md §12: an agent must complete **all 10 tasks via `glm` alone**, with
median session-token cost **≥5x cheaper** than the MCP baseline, before the `sn_*` MCP server
retires. Tasks are drawn from the real weekly workloads (DESIGN.md §13).

## Protocol

1. **Baseline first** (before disabling anything): run each task in a fresh agent session with
   the current MCP fleet (`sn_*` + SmartWork), record total session tokens (input + output).
2. **glm runs**: same tasks, fresh session each, `glm` + its skill only — no SN MCP servers loaded.
3. Same model and same phrasing of the task prompt in both runs. A task counts as completed only
   if the answer is correct (spot-check against the instance).
4. Record in the results table below. Success = 10/10 completed AND median ratio ≥ 5x.

## Fill in before running

- [ ] Profile names for dev + QA instances — dev: `dev` (ven07100, `svc.glm`) · QA: **TBD**
- [x] Scoped-app scope name for tasks 1–5 — `x_n1ll2_smart_gmt`
- [x] A real function/token for the grep task — `processApprovedTimesheet`
- [ ] GRC table names on the instance that holds compliance data (authority documents,
      citations, controls, attestations, risks) — verify actual plugin table names — **TBD**
- [ ] SmartWork table names + one scripted REST endpoint path for task 9 (QA instance) — **TBD**
- [x] A custom table the agent has never been told about, for task 10 — label **"Milestone"**
      (label only, on purpose: its name, schema, and fields stay unrecorded so the
      cold-start test is honest)

## Tasks

### App development

**T1 — Business rules on a table.** List all active business rules on `<table>` (name, when,
order, condition), then pull the **full script** of one of them by name.
*Exercises: query default fields, `get` by human key, `--full`.*

**T2 — Code search.** Find every business rule, script include, and client script referencing
`<token>`; show table, name, and matched lines only.
*Exercises: `glm grep`, `--scope` filter.*

**T3 — Schema verification.** For scoped table `<x_table>`, show all fields (type, reference
target, mandatory) including inherited ones, then confirm whether fields `<a>`, `<b>`, `<c>`
exist before querying them.
*Exercises: `glm schema`, pre-flight field validation (did-you-mean on the miss).*

**T4 — Update set review.** List in-progress update sets for scope `<x_scope>`, then summarize
what a named update set captured, grouped by artifact type.
*Exercises: query + `agg --group-by` on `sys_update_set`/`sys_update_xml`.*

**T5 — Log tail.** Show errors and warnings from the last 15 minutes for source/scope
`<pattern>`, newest first.
*Exercises: `--since 15m`, ordering, stderr pagination hints.*

### Compliance

**T6 — NIST CSF v2.0 trace.** From the NIST CSF v2.0 authority document, walk citations →
control objectives → controls for one function (e.g. PR.AA): show the chain with record counts
at each hop and the controls' implementation states.
*Exercises: multi-hop reference walking, `--format ids` | stdin batch `get`, dot-walking.*

**T7 — Executive posture.** Produce: (a) control coverage — implemented vs total by control
family; (b) top attestation gaps — open/overdue attestations grouped by owner or family;
(c) the 10 riskiest open items by score. Agent synthesizes a short executive summary from the
three result sets.
*Exercises: `agg` (group-by, count), `query --limit` with ordering — the "compose primitives
instead of bespoke summary tools" bet.*

### SmartWork

**T8 — My open work.** All LOs and TODOs assigned to me with states, plus the full hierarchy
under one named North Star.
*Exercises: generic queries against SmartWork tables, zero SmartWork-specific code.*

**T9 — SmartWork action via API (QA).** Transition an LO (or log time) through its scripted
REST endpoint, then verify the resulting state with a query.
*Exercises: `glm api` non-GET flow (request preview + `--yes`), read-after-write.*

### Cross-cutting

**T10 — Cold-start discovery.** On a custom table the agent has never been told about: find it
by label, inspect its schema, and return the 5 most recent records with sensible columns —
zero configuration, zero prior knowledge.
*Exercises: `glm tables`, `glm schema`, auto-derived default fields — the zero-config promise
end to end.*

## Task prompts (use verbatim in both runs)

Fresh session per task, same model, tool-neutral phrasing — the session's available tooling
(MCP fleet vs. glm + skill) is the only variable. Spot-check answers before recording.

- **T1:** On our dev ServiceNow instance, list all active business rules on the
  x_n1ll2_smart_gmt_todo table with name, when they run, order, and condition. Then show me
  the complete script of the one named "SmartWork - Validate Todo Order".
- **T2:** Search our dev instance for every business rule, script include, and client script
  that references processApprovedTimesheet. Show table, record name, and the matching lines only.
- **T3:** On our dev instance, show every field on the x_n1ll2_smart_gmt_timesheet table
  including inherited ones, with type, reference target, and mandatory flag. Then tell me
  whether the fields week_ending, total_hours, and approver exist, and list the 5 most recent
  timesheets using whichever of those exist.
  *(Ground truth: week_ending and total_hours exist; "approver" does not — the real field is
  approved_by, so this also scores the did-you-mean experience.)*
- **T4:** On our dev instance, list in-progress update sets for the x_n1ll2_smart_gmt
  application, then summarize what the update set named "Travis Curtsinger" captured, grouped
  by artifact type.
- **T5:** Show error and warning log entries from the last 15 minutes on our dev instance
  whose source starts with x_n1ll2, newest first.
- **T6 — TBD** pending GRC table names.
- **T7 — TBD** pending GRC table names.
- **T8 — TBD** pending a North Star name and the user whose work to list ("assigned to me"
  must name a real person — the benchmark session authenticates as svc.glm).
- **T9 — TBD** pending the QA profile and scripted REST endpoint path.
- **T10:** There's a custom table somewhere on our dev instance whose label is roughly
  "Milestone". Find it, show me its structure, and give me the 5 most recent records with
  sensible columns.

## Results

| Task | MCP baseline tokens | glm tokens | Ratio | Completed? | Notes |
|------|--------------------:|-----------:|------:|------------|-------|
| T1   |                     |            |       |            |       |
| T2   |                     |            |       |            |       |
| T3   |                     |            |       |            |       |
| T4   |                     |            |       |            |       |
| T5   |                     |            |       |            |       |
| T6   |                     |            |       |            |       |
| T7   |                     |            |       |            |       |
| T8   |                     |            |       |            |       |
| T9   |                     |            |       |            |       |
| T10  |                     |            |       |            |       |

**Median ratio:** ___ (target ≥ 5x) · **Completed:** ___/10
