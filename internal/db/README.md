# Database model and generated client

`schema` is the reviewed, tracked Ent model. `mise run ent-generate` generates
the corresponding runtime client into `ent` using the isolated root
`entc.mod`. The `ent` directory is deliberately ignored: build, test, lint,
license, and development mise tasks generate it before compiling.

Ent automatic migration is disabled. Runtime schema creation remains owned by
the reviewed SQLite and PostgreSQL baseline SQL embedded by `internal/meta`.
