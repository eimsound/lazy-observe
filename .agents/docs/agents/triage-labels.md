# Triage Labels

The skills speak in terms of canonical triage roles. Each triaged issue carries exactly one
**category** role and one **state** role. This file maps those roles to the actual label strings
used in this repo's issue tracker.

| Role in mattpocock/skills | Label in our tracker | Kind     | Meaning                                  |
| ------------------------- | -------------------- | -------- | ---------------------------------------- |
| `bug`                     | `bug`                | category | Something is broken                      |
| `enhancement`             | `enhancement`        | category | New feature or improvement               |
| `needs-triage`            | `needs-triage`       | state    | Maintainer needs to evaluate this issue  |
| `needs-info`              | `needs-info`         | state    | Waiting on reporter for more information |
| `ready-for-agent`         | `ready-for-agent`    | state    | Fully specified, ready for an AFK agent  |
| `ready-for-human`         | `ready-for-human`    | state    | Requires human implementation            |
| `wontfix`                 | `wontfix`            | state    | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label
string from this table.

All 7 canonical labels above (`bug`, `enhancement`, `needs-triage`, `needs-info`,
`ready-for-agent`, `ready-for-human`, `wontfix`) are created and maintained on
`eimsound/lazy-observe` (and `.github/ISSUE_TEMPLATE/{bug_report,feature_request}.yml`
automatically apply `needs-triage` alongside the category label). If a future skill introduces a new
role, create the label with `gh label create <label> --color <hex>` and add it to the table above.
