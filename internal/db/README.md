# Database model and generated client

`schema` is the reviewed Ent model. `mise run ent-generate` generates the
corresponding runtime client into `ent` using the isolated root `entc.mod`.
The `ent` directory is generated code tracked in git: after changing `schema`,
run `mise run ent-generate` and commit the result together. `mise run
ent-generate-check` (also run by CI) fails when the tracked client is stale.

Ent automatic migration is disabled. Runtime schema creation remains owned by
the reviewed SQLite and PostgreSQL baseline SQL embedded by `internal/meta`.
