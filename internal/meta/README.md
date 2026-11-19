# Metadata database

The hand-written metadata service lives in this package. The Ent model is in
`internal/db/schema`; its generated client is tracked in git under
`internal/db/ent` and refreshed by `mise run ent-generate`. Generated Ent source
is not stored in `internal/meta`.

lazy-observe uses SQLite by default. Leave `meta.db_url` empty for the zero-configuration database at `data_dir/meta/lazy-observe.db`; this is the right choice for a single-node installation and keeps directory backup/restore simple. A custom SQLite file can be selected with `sqlite://path/to/meta.db`.

Use PostgreSQL when an existing database service, centralized backups, or DBA-managed operations are more important than the embedded deployment. Configure one standard connection URL:

```yaml
meta:
  db_url: "postgres://user:password@db.example.com:5432/lazy_observe?sslmode=require"
```

The same value can be supplied with `--meta-db-url` or `LO_META_DB_URL`. Credentials are not logged by the metadata package. The PostgreSQL database must already exist and the configured user must be able to create and alter tables, indexes, and constraints in its `public` schema.

## Migrations

`OpenStore` determines the dialect from the URL, then creates an empty database from the embedded baseline before constructing the shared ent client. SQLite uses `migrations/001_baseline.sql`; PostgreSQL uses `migrations/postgres/001_baseline.sql`. Before the first supported release these paired files are mutable and describe the complete current schema. A single transactional `schema_version` row records the baseline name and checksum; it is not a supported version-to-version upgrade history. Runtime auto-migration is forbidden.

SQLite remains limited to one open database connection and keeps its foreign-key and busy-timeout settings. PostgreSQL uses pgx v5 through `database/sql`. Issue aggregation retains the process-level single-writer batch boundary on both databases.

Normal tests always exercise SQLite and skip PostgreSQL. Run the real PostgreSQL suite with:

```sh
mise run test-meta-pg
```

The opt-in suite uses `embedded-postgres`, migrates isolated real PostgreSQL databases, compares the migrated table/column/nullability shape with SQLite, and runs CRUD plus Issue batching/lifecycle checks on both dialects. The first run downloads a PostgreSQL binary into the library cache.

## Pre-release baseline contract

[ADR 0003](../../.agents/docs/adr/0003-pre-release-single-database-baseline.md)
defines the current policy. There is no supported database upgrade path during
initial development. Schema changes are folded into both `001_baseline.sql`
files and existing development databases must be recreated.

The startup sequence remains fail-closed:

1. Open the database but remain unready.
2. For PostgreSQL, acquire a database-scoped advisory lock on a dedicated
   connection with a bounded wait. SQLite remains single-process only.
3. Create/read the baseline marker under that coordination.
4. On an empty database, apply the complete baseline and its marker in one
   transaction. On an initialized database, require the same baseline checksum.
5. Release the lock only after completion; any failure aborts initialization.

This preserves safe concurrent first startup without claiming compatibility
with databases produced by earlier development commits. Ent automatic
migration, Atlas, and a separate migration CLI do not participate.

PostgreSQL first-run seed data uses a separate database-specific advisory lock.
`Bootstrap` and `BootstrapSelfMonitoring` therefore remain idempotent when
application replicas start concurrently after baseline initialization; SQLite retains its
single-process bootstrap model.

## Alert-engine coordination

The baseline includes engine membership, per-rule leases, and durable rule
state. Membership and lease expiry are evaluated by
database time. A lease keeps its epoch only for an unexpired same-owner renewal;
expiry or owner change increments it. `PutAlertRuleState` performs its lease
check and state upsert in one statement, so a stale process cannot persist a
pending, firing, or recovered transition after ownership moves.

The baseline also includes durable notification work. `EnqueueNotification`
is idempotent; PostgreSQL claims use row locking with
`SKIP LOCKED`, while SQLite keeps its single-process transaction. Claims carry
an expiry for crash recovery. Completion requires the current claim owner;
failed sends retry with a future availability time and become `dead` after the
configured attempt bound. Rate-limit deferral releases a claim without
consuming an attempt.

PostgreSQL pins one connection and acquires a database-specific advisory lock
before touching the baseline marker. This is application-process coordination
over one shared writable database, not a requirement for PostgreSQL database
replicas and not a rolling schema-upgrade promise.

The accepted decisions are recorded in
[ADR 0001](../../.agents/docs/adr/0001-nightingale-style-alert-engine.md) and
[ADR 0003](../../.agents/docs/adr/0003-pre-release-single-database-baseline.md).

Before the first supported release, an incompatible schema change requires
recreating the development database. A later release decision will freeze the
baseline and define the append-only upgrade and rollback contract.
