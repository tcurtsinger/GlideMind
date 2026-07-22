---
name: glm
description: Query ServiceNow with the glm CLI — records, schema, counts, aggregates, code search, attachments, raw REST. Use whenever a task needs ServiceNow data.
---

Run `glm prime` once to load the command cheatsheet, then compose glm commands directly.

Essentials:

- Discover structure first: `glm tables <pattern>`, then `glm schema <table>`.
- Filters are native ServiceNow encoded queries; repeat `-q` to AND clauses.
- Data arrives on stdout; summaries, pagination hints, and warnings on stderr.
- Truncated values end in a marker naming the exact follow-up command — never dead-end.
- `--profile <name>` selects the instance; `glm whoami` sanity-checks auth.
