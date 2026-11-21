---
name: implement
description: "Implement a piece of work based on a spec or set of tickets."
disable-model-invocation: true
---

Implement the work described by the user in the spec or tickets.

If the user passes a ticket reference, fetch it from the issue tracker and state its title before starting. If the reference is ambiguous, ask.

Before designing or editing code or CI/deploy config, read `.agents/skills/pitfalls/SKILL.md` and the matching surface file(s) (skip only for typo, comment, or doc-only edits).

Call the Skill tool with "tdd" when adding or changing testable behavior at pre-agreed seams.

Run typechecking regularly, single test files regularly, and the full test suite once at the end.

Once done, call the Skill tool with "code-review" to review the work.

Commit your work to the current branch.
