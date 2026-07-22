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
5. Run every benchmark session **outside this repo's working directory** — this file contains
   evaluator-only ground truth an agent could otherwise read.

## Fill in before running

- [ ] Profile names for dev + QA instances — dev: `dev` (ven07100, `svc.glm`) · QA: **TBD**
      (recon suggests ven04204 but could not confirm — the QA SmartWork MCP resolves a
      ven07100 identity; verify the real QA hostname before creating the profile)
- [x] Scoped-app scope name for tasks 1–5 — `x_n1ll2_smart_gmt`
- [x] A real function/token for the grep task — `processApprovedTimesheet`
- [x] GRC table names (verified by MCP recon 2026-07-22, all on ven07100):
      `sn_compliance_authority_document` (15) / `sn_compliance_citation` (3,972) /
      `sn_compliance_policy_statement` = control objectives (4,718) /
      `sn_compliance_control` (10,584). No dedicated attestation table — the generic
      `asmt_assessment_instance` engine holds the assessments (1,985). `sn_risk_risk` is empty.
- [x] SmartWork tables + T9 endpoint (verified by MCP recon): hierarchy is North Star →
      Milestone → LO → To-Do (`x_n1ll2_smart_gmt_north_star` / `_milestone` /
      `_leveraged_outcome` / `_todo`); T9 endpoint: **POST
      /api/x_n1ll2_smart_gmt/smartwork_cc/todo/log_time** (SmartWorkTimeService.logTime).
      Still gated on the QA profile above.
- [x] A custom table the agent has never been told about, for task 10 — label **"Milestone"**
      (schema and contents stay unrecorded; benchmark sessions must not read this file —
      protocol rule 5)

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

**T5 — Log tail.** Show log entries from a recent window for source/scope `<pattern>`,
newest first, separating errors/warnings from informational noise.
*Exercises: `--since`, ordering, stderr pagination hints. (Recon: this app logs ~120
info entries/day and zero errors — a 15-minute error-only window would be deterministically
empty, so the task uses a 24-hour window and asks for the level breakdown.)*

### Compliance

**T6 — Framework trace.** From the NIST SP 800-53 Rev 5.1.1 authority document, walk
citations → control objectives → controls for one control family (e.g. AC): show the chain
with record counts at each hop and the controls' implementation states.
*Exercises: multi-hop reference walking, `--format ids` | stdin batch `get`, dot-walking.
(Recon: no NIST CSF v2.0 authority document exists on this instance — re-based onto the
fully-loaded 800-53 Rev 5.1.1 corpus.)*

**T7 — Executive posture.** Produce: (a) control coverage — implemented vs total, grouped
by authority document or family; (b) top assessment gaps — open/overdue assessment instances
grouped by assignee; (c) the 10 control objectives with the most not-yet-implemented controls.
Agent synthesizes a short executive summary from the three result sets.
*Exercises: `agg` (group-by, count), `query --limit` with ordering — the "compose primitives
instead of bespoke summary tools" bet. (Recon: `sn_risk_risk` is empty, so the original
riskiest-items leg is replaced with the control-objective gap list.)*

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
  in the x_n1ll2_smart_gmt application that references processApprovedTimesheet. Show table,
  record name, and the matching lines only.
- **T3:** On our dev instance, show every field on the x_n1ll2_smart_gmt_timesheet table
  including inherited ones, with type, reference target, and mandatory flag. Then tell me
  whether the fields week_ending, total_hours, and approver exist, and list the 5 most recent
  timesheets using whichever of those exist.
- **T4:** On our dev instance, list in-progress update sets for the x_n1ll2_smart_gmt
  application, then summarize what the in-progress update set named "Travis Curtsinger" in
  the C1 SmartGTM application captured, grouped by artifact type.
- **T5:** Show log entries from the last 24 hours on our dev instance whose source starts
  with x_n1ll2, newest first — and tell me how many are errors or warnings versus
  informational.
- **T6:** On our dev instance, starting from the NIST SP 800-53 Rev 5.1.1 authority document
  (AD0020005), walk the chain for the Access Control (AC) family: its citations, their
  control objectives, and the controls under those objectives. Show record counts at each
  hop and break down the controls by implementation state.
- **T7:** From our dev instance's compliance data, produce three result sets and then a
  short executive summary: (a) control coverage — how many controls are implemented versus
  total, grouped by authority document or control family; (b) assessment gaps — open or
  overdue assessment instances grouped by assignee; (c) the 10 control objectives with the
  most controls that are not yet implemented.
- **T8:** On our dev instance, show all SmartWork Leveraged Outcomes and To-Dos owned by
  Travis Curtsinger with their states. Then show the full hierarchy under the North Star
  named "Leidos IRM Evolution: The AI-Powered DER Engine" — its milestones, their leveraged
  outcomes, and their to-dos, with counts at each level.
- **T9:** On our QA instance, log 1 hour of time against an open SmartWork To-Do through the
  SmartWork Command Center scripted REST API (POST
  /api/x_n1ll2_smart_gmt/smartwork_cc/todo/log_time — inspect the operation to determine the
  body it expects), then verify with a query that the time entry was recorded.
- **T10:** There's a custom table somewhere on our dev instance whose label is roughly
  "Milestone". Find it, show me its structure, and give me the 5 most recent records with
  sensible columns.

### Evaluator-only notes (never pasted into a session)

All ground truth below verified 2026-07-22 by an independent MCP-equipped recon session.

- **T1:** 19 active business rules on the To-Do table; "SmartWork - Validate Todo Order"
  exists (before, order 100).
- **T2:** exactly 3 records reference the token — a script include defining it
  (BillingPeriodUtils), a script include calling it (SmartWorkPortalUtils), and a UI action
  (Approve Timesheet). No business rules or client scripts.
- **T3:** week_ending and total_hours exist; "approver" does not — the real field is
  approved_by. A correct run notices the miss (ideally surfacing a did-you-mean) and lists
  timesheets using the two real fields.
- **T4:** FOUR in-progress update sets share the name (one per application). The C1 SmartGTM
  one holds 31 records: cross scope privilege ×19, dictionary ×4, field label ×3, choice
  list ×2, form layout ×1, restricted caller access privilege ×1, table ×1.
- **T5:** expect zero errors/warnings and ~120+ informational entries in a typical 24-hour
  window; honestly reporting "no errors or warnings" is the correct answer, not a failure.
- **T6:** instance totals: 15 authority documents, 3,972 citations, 4,718 control
  objectives, 10,584 controls. AD0020005's full name is "Electronic Version of NIST SP
  800-53 Rev 5.1.1 Controls and SP 800-53A Rev 5.1.1 Assessment Procedures".
- **T7:** assessments live in the generic asmt_assessment_instance table (1,985 records);
  there is no dedicated GRC attestation table and sn_risk_risk is empty (the risk leg was
  re-scoped deliberately).
- **T8:** expected shape — 4 milestones, 18 LOs, 84 to-dos under that North Star (102
  descendants); travisc owns 1 open LO and 7 open to-dos (assignment field is "owner").
- **T9:** endpoint verified on dev; the operation script calls
  SmartWorkTimeService().logTime(body). Runs on QA only — gated on the QA profile.
- **T10:** multiple custom tables carry a Milestone-ish label (4 custom + 1 baseline); a
  correct run disambiguates — asks, or picks the scoped-app one — and returns its 5 most
  recent records with sensible columns.

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
