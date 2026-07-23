---
name: glm
description: "Query and inspect ServiceNow with the `glm` CLI — records (incidents, changes, tasks, CMDB CIs, catalog items, users, any table), schema and field checks, counts and aggregates, server-side code search across script tables, attachments, and raw REST. Use whenever a task touches ServiceNow data: reading or filtering records, verifying a table's fields before querying, tracing reference/dot-walk chains, searching business rules or script includes, pulling compliance/GRC data, or hitting any `/api/now/...` endpoint — even when the user never mentions glm by name. Prefer it over raw REST calls or ServiceNow MCP tools; it answers the same questions in far fewer tokens."
---

# glm — ServiceNow, in the fewest tokens

`glm` is a context-economical ServiceNow CLI. Every default is chosen to answer a
ServiceNow question with minimal output, so reach for it over raw REST or MCP tools
whenever you need instance data.

## First move: `glm prime`

Run `glm prime` at the start of ServiceNow work. It prints the authoritative command
list with synopses, generated from the binary itself — always current, never drifts.
Treat it as the command reference; this skill gives you the mental model and patterns
prime's terse output can't. (If the shell can't find `glm`, it may not be on PATH — ask
the user rather than guessing at a full path.)

## The economy mindset (why glm exists)

Work with the grain of the tool — it is built to spend the fewest tokens:

- **Reduce before you read.** `count` and `agg` answer "how many?" and "breakdown by X?"
  without pulling records at all. `--format ids | glm get <table> - --json` fetches full
  detail only for the rows you actually need. `--fields a,b` narrows columns; with no
  `--fields`, glm derives sensible defaults from the table's schema.
- **Batch.** Independent glm commands can share one shell call — cheaper than a turn each.
- **stdout is data; stderr is metadata.** Rows go to stdout; row counts, pagination hints
  (`next: --offset 25`), the "N of M columns shown" note, and warnings go to stderr — so
  pipes stay clean and you still see the guidance.

## Discovery — you don't need to know a table exists

1. `glm tables <pattern>` — find tables by name or label.
2. `glm schema <table>` — fields, types, reference targets, mandatory flags, plus the
   inheritance chain and cache age on stderr.
3. Query it. Field names in `-q`/`--fields` are checked against the schema first, so a typo
   fails with a did-you-mean instead of ServiceNow's silent empty result (the API returns
   empty strings for fields that don't exist — the classic footgun). Trust the check; if you
   just created a field and it's flagged unknown, glm refetches the schema once and heals.

## Queries are native encoded queries

No invented DSL — the same syntax the platform uses:

- `-q` is repeatable and AND-joined (`-q active=true -q priority=1`), which also spares you
  from shell-quoting the `^` separator.
- Copy a query from the platform UI's "Copy query" and pass it verbatim.
- Filter on **stored values, not display labels**: `-q state=2`, not `-q state="In Progress"`.
- Dot-walk passes straight through: `-q assigned_to.department.name=Engineering`.

## Recipes — real task → command

Combine these freely; they are patterns, not an exhaustive flag list (that's `glm prime`).

- **Active P1 incidents** → `glm query incident -q active=true -q priority=1`
- **The full script of a business rule by name** → `glm get sys_script "Incident autoclose" --fields script --full`
- **Where a function/class is used in code** → `glm grep processApprovedTimesheet`
  (searches business rules, script includes, client scripts, UI actions, UI policies)
- **Coverage / posture breakdown** → `glm agg sn_compliance_control --group-by state`
- **A plain count** → `glm count incident -q active=true`
- **Recent activity / log tail** → `glm query syslog --since 15m --order-by -sys_created_on`
- **Full detail for a filtered set (multi-hop)** →
  `glm query incident -q priority=1 --format ids | glm get incident - --json`
- **An endpoint glm has no verb for** → `glm api GET /api/now/table/<t> -f sysparm_limit=5`

## Gotchas worth knowing up front

- **`^` can't be a literal value** — it's the encoded-query separator and has no in-value
  escape. glm rejects it; narrow the value to a fragment without `^`.
- **Writes are gated.** `glm api` with POST/PUT/PATCH/DELETE prints the exact request and
  refuses without `--yes` — glm is read-only otherwise.
- **Truncation never dead-ends.** A cut value ends in a marker showing how to get the rest
  (`--full`); `grep`'s remainder marker names the exact `glm get` to run.
- **PowerShell:** quote the stdin body marker — `--body '@-'` (bare `@-` is a parse error).

## Auth

`--profile <name>` (or `-p`) selects the instance; `glm whoami` confirms identity and roles.
Credentials live in the OS keyring per machine, so a fresh machine needs its own
`glm profile add` before any command will authenticate.
