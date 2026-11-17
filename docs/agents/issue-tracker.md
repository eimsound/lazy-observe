# Issue tracker: GitHub

Issues and specs for this repo live as GitHub Issues on `eimsound/lazy-observe`. Use the `gh` CLI for all operations.

## Conventions

- **Create an issue**: `gh issue create --title "..." --body "..."`. Use `--body-file -` with a heredoc for multi-line bodies.
- **Read an issue**: `gh issue view <number>` — add `--comments` to include comments, and `--json title,body,labels,state` to narrow output.
- **List issues**: `gh issue list --state open` with `--label`, `--state`, and `--search` filters as needed.
- **Comment on an issue**: `gh issue comment <number> --body "..."`.
- **Apply labels**: `gh issue edit <number> --add-label "needs-triage"` (remove with `--remove-label`); manage the label set with `gh label` (`list`/`create`/`edit`/`delete`).
- **Close**: `gh issue close <number>`.
- **Assign**: `gh issue edit <number> --add-assignee <user>`.
- **Blocking edges**: write `Blocked by #<number>` in the issue body; GitHub sub-issues may group a spec's tickets under the spec issue.

`gh` infers the repo from `git remote -v` when run inside a clone (use `--repo eimsound/lazy-observe` to override).

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo treats external PRs as feature requests; `/triage` reads this flag.)_

When set to `yes`, PRs run through the same labels and states as issues, using the `gh pr` equivalents.

## When a skill says "publish to the issue tracker"

Create a GitHub issue.

## When a skill says "fetch the relevant ticket"

Run `gh issue view <number> --comments`.
