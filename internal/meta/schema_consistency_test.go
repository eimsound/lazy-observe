package meta

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/profilemeta"
)

var (
	sqlCreateTableRe = regexp.MustCompile(`(?i)^\s*CREATE\s+TABLE\s+([a-z0-9_]+)\s*\(`)
	sqlCreateIndexRe = regexp.MustCompile(
		`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+([a-z0-9_]+)\s+ON\s+([a-z0-9_]+)\s*\(([^)]+)\)`,
	)
)

func TestBaselineSQLDeclarationsMatchAcrossDialects(t *testing.T) {
	extract := func(path string) (tables []string, indexes []string) {
		raw, err := migrationFiles.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Collapse multiline CREATE INDEX definitions into one logical statement.
		for _, stmt := range strings.Split(string(raw), ";") {
			normalized := strings.Join(strings.Fields(strings.TrimSpace(stmt)), " ")
			if normalized == "" {
				continue
			}
			if m := sqlCreateTableRe.FindStringSubmatch(normalized); m != nil {
				tables = append(tables, strings.ToLower(m[1]))
				continue
			}
			if m := sqlCreateIndexRe.FindStringSubmatch(normalized); m != nil {
				unique := strings.TrimSpace(m[1]) != ""
				cols := strings.ReplaceAll(strings.ToLower(m[4]), `"`, "")
				cols = strings.Join(strings.Fields(cols), " ")
				indexes = append(
					indexes,
					fmt.Sprintf("%s.%s:unique=%t(%s)", strings.ToLower(m[3]), strings.ToLower(m[2]), unique, cols),
				)
			}
		}
		sort.Strings(tables)
		sort.Strings(indexes)
		return tables, indexes
	}

	sqliteTables, sqliteIndexes := extract("migrations/001_baseline.sql")
	pgTables, pgIndexes := extract("migrations/postgres/001_baseline.sql")
	if !reflect.DeepEqual(sqliteTables, pgTables) {
		t.Fatalf("baseline tables differ:\nsqlite: %v\npostgres: %v", sqliteTables, pgTables)
	}
	if !reflect.DeepEqual(sqliteIndexes, pgIndexes) {
		t.Fatalf("baseline explicit indexes differ:\nsqlite: %v\npostgres: %v", sqliteIndexes, pgIndexes)
	}
	tableSet := make(map[string]bool, len(sqliteTables))
	for _, tbl := range sqliteTables {
		tableSet[tbl] = true
	}
	for _, required := range []string{"replay_segments", "deleted_project_slugs", "notification_outbox"} {
		if !tableSet[required] {
			t.Fatalf("baseline missing required table %q", required)
		}
	}
}

func TestSingleBaselineSetsMatch(t *testing.T) {
	versions := func(directory string) []string {
		entries, err := migrationEntries(migrationFiles, directory)
		if err != nil {
			t.Fatal(err)
		}
		var result []string
		for _, entry := range entries {
			result = append(result, entry.Name())
		}
		return result
	}
	if sqlite, postgres := versions(
		"migrations",
	), versions(
		"migrations/postgres",
	); !reflect.DeepEqual(
		sqlite,
		postgres,
	) {
		t.Fatalf("baseline sets differ: sqlite=%v postgres=%v", sqlite, postgres)
	} else if len(sqlite) != 1 ||
		sqlite[0] != "001_baseline.sql" {
		t.Fatalf("expected one pre-release baseline, got %v", sqlite)
	}
	for _, directory := range []string{"migrations", "migrations/postgres"} {
		definitions, err := loadMigrationDefinitions(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, definition := range definitions {
			decoded, err := hex.DecodeString(definition.checksum)
			if err != nil || len(decoded) != 32 {
				t.Fatalf("%s migration %d checksum=%q err=%v", directory, definition.version, definition.checksum, err)
			}
		}
	}
}

func TestBaselineMetadataAndChecksumEnforcement(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		directory := "migrations"
		if store.dialect == "postgres" {
			directory = "migrations/postgres"
		}
		definitions, err := loadMigrationDefinitions(directory)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := store.db.Query(
			`SELECT version, name, checksum, app_version, duration_ms FROM schema_version ORDER BY version`,
		)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for rows.Next() {
			var version int
			var durationMillis int64
			var name, checksum, appVersion string
			if err := rows.Scan(&version, &name, &checksum, &appVersion, &durationMillis); err != nil {
				t.Fatal(err)
			}
			if seen >= len(definitions) {
				t.Fatalf("unexpected extra migration history row version=%d", version)
			}
			definition := definitions[seen]
			if version != definition.version || name != definition.name || checksum != definition.checksum ||
				appVersion == "" ||
				durationMillis < 0 {
				t.Fatalf(
					"history row=(%d,%q,%q,%q,%d) definition=%#v",
					version,
					name,
					checksum,
					appVersion,
					durationMillis,
					definition,
				)
			}
			seen++
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if seen != len(definitions) {
			t.Fatalf("history rows=%d want=%d", seen, len(definitions))
		}
		if _, err := store.db.Exec(
			bind(store.dialect, `UPDATE schema_version SET checksum = ? WHERE version = 1`),
			strings.Repeat("0", 64),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStore(
			context.Background(),
			store.dbURL,
		); err == nil ||
			!strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("checksum mismatch error=%v", err)
		}
	})
}

func TestMigratedSchemaEquivalent(t *testing.T) {
	if postgresAdminURL == "" {
		t.Skip("set LO_TEST_PG=1 to compare SQLite and PostgreSQL schemas")
	}
	ctx := context.Background()
	sqlite, err := OpenStore(ctx, t.TempDir()+"/meta.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	postgres := newPostgresTestStore(t)
	want := schemaColumns(t, sqlite)
	got := schemaColumns(t, postgres)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("migrated table/column/type/nullability/default shape differs:\nsqlite: %v\npostgres: %v", want, got)
	}
	wantPKs := schemaPrimaryKeys(t, sqlite)
	gotPKs := schemaPrimaryKeys(t, postgres)
	if !reflect.DeepEqual(wantPKs, gotPKs) {
		t.Fatalf("migrated primary keys differ:\nsqlite: %v\npostgres: %v", wantPKs, gotPKs)
	}
	wantUniques := schemaUniqueConstraints(t, sqlite)
	gotUniques := schemaUniqueConstraints(t, postgres)
	if !reflect.DeepEqual(wantUniques, gotUniques) {
		t.Fatalf("migrated unique constraints differ:\nsqlite: %v\npostgres: %v", wantUniques, gotUniques)
	}
	wantIndexes := schemaIndexes(t, sqlite)
	gotIndexes := schemaIndexes(t, postgres)
	if !reflect.DeepEqual(wantIndexes, gotIndexes) {
		t.Fatalf("migrated indexes differ:\nsqlite: %v\npostgres: %v", wantIndexes, gotIndexes)
	}
	wantFKs := schemaForeignKeys(t, sqlite)
	gotFKs := schemaForeignKeys(t, postgres)
	if !reflect.DeepEqual(wantFKs, gotFKs) {
		t.Fatalf("migrated foreign keys differ:\nsqlite: %v\npostgres: %v", wantFKs, gotFKs)
	}
	assertSchemaCRUD(t, sqlite)
	assertSchemaCRUD(t, postgres)
}

func schemaPrimaryKeys(t *testing.T, store *Store) []string {
	t.Helper()
	ctx := context.Background()
	var result []string
	if store.dialect == "postgres" {
		rows, err := store.db.QueryContext(ctx, `
			SELECT t.relname,
				array_to_string(array(
					SELECT a.attname
					FROM unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord)
					JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
					ORDER BY k.ord
				), ',')
			FROM pg_index ix
			JOIN pg_class t ON t.oid = ix.indrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = 'public'
			  AND t.relname <> 'schema_version'
			  AND ix.indisprimary
			ORDER BY t.relname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var table, cols string
			if err := rows.Scan(&table, &cols); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s:(%s)", table, cols))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(result)
		return result
	}
	tables, err := store.db.QueryContext(
		ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY name`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	for _, table := range names {
		rows, err := store.db.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
		if err != nil {
			t.Fatal(err)
		}
		type pkCol struct {
			pos  int
			name string
		}
		var pkCols []pkCol
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			if pk > 0 {
				pkCols = append(pkCols, pkCol{pos: pk, name: name})
			}
		}
		_ = rows.Close()
		if len(pkCols) > 0 {
			sort.Slice(pkCols, func(i, j int) bool { return pkCols[i].pos < pkCols[j].pos })
			cols := make([]string, len(pkCols))
			for i, c := range pkCols {
				cols[i] = c.name
			}
			result = append(result, fmt.Sprintf("%s:(%s)", table, strings.Join(cols, ",")))
		}
	}
	sort.Strings(result)
	return result
}

func schemaUniqueConstraints(t *testing.T, store *Store) []string {
	t.Helper()
	ctx := context.Background()
	var result []string
	if store.dialect == "postgres" {
		rows, err := store.db.QueryContext(ctx, `
			SELECT t.relname,
				array_to_string(array(
					SELECT a.attname || CASE WHEN (ix.indoption[k.ord - 1] & 1) = 1 THEN ' DESC' ELSE '' END
					FROM unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord)
					JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
					ORDER BY k.ord
				), ',')
			FROM pg_index ix
			JOIN pg_class t ON t.oid = ix.indrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = 'public'
			  AND t.relname <> 'schema_version'
			  AND ix.indisunique
			  AND NOT ix.indisprimary
			ORDER BY t.relname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var table, cols string
			if err := rows.Scan(&table, &cols); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s:(%s)", table, cols))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(result)
		return result
	}
	tables, err := store.db.QueryContext(
		ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY name`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	for _, table := range names {
		idxRows, err := store.db.QueryContext(ctx, `PRAGMA index_list("`+table+`")`)
		if err != nil {
			t.Fatal(err)
		}
		var uniqueIdxNames []string
		for idxRows.Next() {
			var seq, unique, partial int
			var name, origin string
			if err := idxRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				t.Fatal(err)
			}
			if unique == 1 && (origin == "c" || origin == "u") {
				uniqueIdxNames = append(uniqueIdxNames, name)
			}
		}
		_ = idxRows.Close()
		for _, idxName := range uniqueIdxNames {
			infoRows, err := store.db.QueryContext(ctx, `PRAGMA index_xinfo("`+idxName+`")`)
			if err != nil {
				t.Fatal(err)
			}
			var cols []string
			for infoRows.Next() {
				var seqno, cid, desc, key int
				var colName sql.NullString
				var coll string
				if err := infoRows.Scan(&seqno, &cid, &colName, &desc, &coll, &key); err != nil {
					t.Fatal(err)
				}
				if key == 0 || !colName.Valid {
					continue
				}
				if desc == 1 {
					cols = append(cols, colName.String+" DESC")
				} else {
					cols = append(cols, colName.String)
				}
			}
			_ = infoRows.Close()
			result = append(result, fmt.Sprintf("%s:(%s)", table, strings.Join(cols, ",")))
		}
	}
	sort.Strings(result)
	return result
}

func schemaIndexes(t *testing.T, store *Store) []string {
	t.Helper()
	ctx := context.Background()
	var result []string
	if store.dialect == "postgres" {
		rows, err := store.db.QueryContext(ctx, `
			SELECT t.relname, i.relname, ix.indisunique,
				array_to_string(array(
					SELECT a.attname || CASE WHEN (ix.indoption[k.ord - 1] & 1) = 1 THEN ' DESC' ELSE '' END
					FROM unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord)
					JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
					ORDER BY k.ord
				), ',')
			FROM pg_index ix
			JOIN pg_class t ON t.oid = ix.indrelid
			JOIN pg_class i ON i.oid = ix.indexrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			LEFT JOIN pg_constraint c ON c.conindid = ix.indexrelid
			WHERE n.nspname = 'public'
			  AND t.relname <> 'schema_version'
			  AND NOT ix.indisprimary
			  AND c.oid IS NULL
			ORDER BY t.relname, i.relname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var table, name, cols string
			var unique bool
			if err := rows.Scan(&table, &name, &unique, &cols); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s.%s:unique=%t(%s)", table, name, unique, cols))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(result)
		return result
	}
	tables, err := store.db.QueryContext(
		ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY name`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	for _, table := range names {
		idxRows, err := store.db.QueryContext(ctx, `PRAGMA index_list("`+table+`")`)
		if err != nil {
			t.Fatal(err)
		}
		type sqliteIndex struct {
			name   string
			unique bool
		}
		var indexes []sqliteIndex
		for idxRows.Next() {
			var seq, unique, partial int
			var name, origin string
			if err := idxRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				t.Fatal(err)
			}
			if origin != "c" {
				continue
			}
			indexes = append(indexes, sqliteIndex{name: name, unique: unique == 1})
		}
		_ = idxRows.Close()
		for _, idx := range indexes {
			infoRows, err := store.db.QueryContext(ctx, `PRAGMA index_xinfo("`+idx.name+`")`)
			if err != nil {
				t.Fatal(err)
			}
			var cols []string
			for infoRows.Next() {
				var seqno, cid, desc, key int
				var colName sql.NullString
				var coll string
				if err := infoRows.Scan(&seqno, &cid, &colName, &desc, &coll, &key); err != nil {
					t.Fatal(err)
				}
				if key == 0 || !colName.Valid {
					continue
				}
				if desc == 1 {
					cols = append(cols, colName.String+" DESC")
				} else {
					cols = append(cols, colName.String)
				}
			}
			_ = infoRows.Close()
			result = append(
				result,
				fmt.Sprintf("%s.%s:unique=%t(%s)", table, idx.name, idx.unique, strings.Join(cols, ",")),
			)
		}
	}
	sort.Strings(result)
	return result
}

func schemaForeignKeys(t *testing.T, store *Store) []string {
	t.Helper()
	ctx := context.Background()
	var result []string
	if store.dialect == "postgres" {
		rows, err := store.db.QueryContext(ctx, `
			SELECT
				src.relname,
				sa.attname,
				dst.relname,
				da.attname,
				CASE c.confdeltype
					WHEN 'a' THEN 'NO ACTION'
					WHEN 'r' THEN 'RESTRICT'
					WHEN 'c' THEN 'CASCADE'
					WHEN 'n' THEN 'SET NULL'
					WHEN 'd' THEN 'SET DEFAULT'
					ELSE c.confdeltype::text
				END
			FROM pg_constraint c
			JOIN pg_class src ON src.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = src.relnamespace
			JOIN pg_class dst ON dst.oid = c.confrelid
			JOIN LATERAL unnest(c.conkey, c.confkey) AS cols(src_att, dst_att) ON TRUE
			JOIN pg_attribute sa ON sa.attrelid = src.oid AND sa.attnum = cols.src_att
			JOIN pg_attribute da ON da.attrelid = dst.oid AND da.attnum = cols.dst_att
			WHERE c.contype = 'f' AND n.nspname = 'public' AND src.relname <> 'schema_version'
			ORDER BY src.relname, sa.attname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var srcTable, srcCol, dstTable, dstCol, onDelete string
			if err := rows.Scan(&srcTable, &srcCol, &dstTable, &dstCol, &onDelete); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s.%s->%s.%s:%s", srcTable, srcCol, dstTable, dstCol, onDelete))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(result)
		return result
	}
	tables, err := store.db.QueryContext(
		ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY name`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	for _, table := range names {
		fkRows, err := store.db.QueryContext(ctx, `PRAGMA foreign_key_list("`+table+`")`)
		if err != nil {
			t.Fatal(err)
		}
		for fkRows.Next() {
			var id, seq int
			var refTable, fromCol, toCol, onUpdate, onDelete, match string
			if err := fkRows.Scan(&id, &seq, &refTable, &fromCol, &toCol, &onUpdate, &onDelete, &match); err != nil {
				t.Fatal(err)
			}
			result = append(
				result,
				fmt.Sprintf("%s.%s->%s.%s:%s", table, fromCol, refTable, toCol, strings.ToUpper(onDelete)),
			)
		}
		_ = fkRows.Close()
	}
	sort.Strings(result)
	return result
}

func canonicalColumnType(dialectName, raw string) string {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if dialectName == "postgres" {
		switch normalized {
		case "integer", "bigint", "smallint":
			return "int"
		case "text", "character varying":
			return "text"
		case "boolean":
			return "bool"
		case "timestamp with time zone", "timestamp without time zone":
			return "timestamp"
		case "jsonb", "json":
			return "json"
		case "bytea":
			return "bytes"
		case "double precision", "real":
			return "float"
		default:
			return normalized
		}
	}
	switch normalized {
	case "integer", "int", "bigint":
		return "int"
	case "text":
		return "text"
	case "boolean", "bool":
		return "bool"
	case "timestamp", "datetime":
		return "timestamp"
	case "json":
		return "json"
	case "blob":
		return "bytes"
	case "real", "double":
		return "float"
	default:
		return normalized
	}
}

func normalizeColumnDefault(raw sql.NullString) string {
	if !raw.Valid {
		return ""
	}
	val := strings.TrimSpace(raw.String)
	if idx := strings.Index(val, "::"); idx != -1 {
		val = strings.TrimSpace(val[:idx])
	}
	switch strings.ToLower(val) {
	case "true":
		return "true"
	case "false":
		return "false"
	default:
		return val
	}
}

func schemaColumns(t *testing.T, store *Store) []string {
	t.Helper()
	ctx := context.Background()
	var result []string
	if store.dialect == "postgres" {
		rows, err := store.db.QueryContext(ctx, `
			SELECT table_name, column_name, data_type, is_nullable, column_default
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name <> 'schema_version'
			ORDER BY table_name, ordinal_position`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var table, column, dataType, nullable string
			var colDefault sql.NullString
			if err := rows.Scan(&table, &column, &dataType, &nullable, &colDefault); err != nil {
				t.Fatal(err)
			}
			result = append(
				result,
				fmt.Sprintf(
					"%s.%s:%s:null=%t:def=%s",
					table,
					column,
					canonicalColumnType("postgres", dataType),
					nullable == "YES",
					normalizeColumnDefault(colDefault),
				),
			)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(result)
		return result
	}
	tables, err := store.db.QueryContext(
		ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY name`,
	)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_ = tables.Close()
	for _, table := range names {
		rows, err := store.db.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			result = append(
				result,
				fmt.Sprintf(
					"%s.%s:%s:null=%t:def=%s",
					table,
					name,
					canonicalColumnType("sqlite", typ),
					notNull == 0 && pk == 0,
					normalizeColumnDefault(defaultValue),
				),
			)
		}
		_ = rows.Close()
	}
	sort.Strings(result)
	return result
}

func assertSchemaCRUD(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 34, 56, 123456000, time.UTC)
	p, err := store.client.Project.Create().SetName("Schema smoke").SetSlug("schema-smoke").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.client.NotificationChannel.Create().
		SetType("webhook").
		SetName("schema-webhook").
		SetConfig(map[string]any{"url": "https://example.invalid/hook"}).
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.client.AlertRule.Create().
		SetProjectID(p.ID).
		SetName("schema-rule").
		SetExpr("up == 0").
		SetLabels(map[string]string{"team": "core"}).
		SetAnnotations(map[string]string{"summary": "down"}).
		AddChannelIDs(channel.ID).
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.AlertEvent.Create().
		SetRuleID(rule.ID).
		SetFingerprint("schema-event").
		SetStatus("firing").
		SetLabels(map[string]string{"a": "b"}).
		SetAnnotations(map[string]string{}).
		SetStartsAt(now).
		Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.Silence.Create().
		SetProjectID(p.ID).
		SetMatchers([]map[string]string{{"name": "service", "value": "api"}}).
		SetStartsAt(now).
		SetEndsAt(now.Add(time.Hour)).
		SetCreator("test").
		Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.InhibitionRule.Create().
		SetProjectID(p.ID).
		SetSourceMatchers([]map[string]string{{"name": "severity", "value": "critical"}}).
		SetTargetMatchers([]map[string]string{{"name": "severity", "value": "warning"}}).
		SetEqualLabels([]string{"service"}).
		Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.ProjectAlertConfig.Create().
		SetProjectID(p.ID).
		AddChannelIDs(channel.ID).
		Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.ScrapeTarget.Create().
		SetProjectID(p.ID).
		SetName("schema-target").
		SetURL("http://127.0.0.1:9090/metrics").
		SetLabels(map[string]string{"env": "test"}).
		Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.SourceMapArtifact.Create().
		SetProjectID(p.ID).
		SetRelease("v1").
		SetFilename("app.js").
		SetSize(12).
		SetSha256(strings.Repeat("a", 64)).
		SetCreatedAt(now).
		Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.client.ProfileMeta.Create().
		SetProjectID(p.ID).
		SetUploadID(strings.Repeat("b", 32)).
		SetPayloadSha256(strings.Repeat("c", 64)).
		SetService("api").
		SetType("cpu").
		SetStart(now).
		SetEnd(now.Add(time.Minute)).
		SetSize(42).
		SetBlobKey("schema-profile").
		SetLabels(map[string]string{"node": "one"}).
		Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertReplaySession(
		ctx,
		p.ID,
		"schema-session",
		now,
		now,
		1,
		10,
		false,
		"v1",
		"https://example.invalid",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertReplaySession(
		ctx,
		p.ID,
		"schema-session",
		now.Add(time.Second),
		now.Add(time.Second),
		2,
		20,
		true,
		"",
		"",
	); err != nil {
		t.Fatal(err)
	}
	replay, err := store.GetReplaySession(ctx, p.ID, "schema-session")
	if err != nil || replay.SegmentCount != 2 || !replay.HasError {
		t.Fatalf("replay upsert row=%#v err=%v", replay, err)
	}
	if _, err := store.UpsertReplaySegment(ctx, ReplaySegmentInput{
		ProjectID:     p.ID,
		SessionID:     "schema-seg-session",
		Seq:           0,
		StartedAt:     now,
		LastSeenAt:    now.Add(time.Second),
		EventCount:    3,
		SizeBytes:     64,
		PayloadSHA256: strings.Repeat("d", 64),
		MarkersJSON:   `[{"type":"error","time":1}]`,
		HasError:      true,
		Release:       "v1",
		PageURL:       "https://example.invalid/checkout",
		MaxSegments:   200,
		MaxBytes:      50 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	segs, err := store.ListReplaySegments(ctx, p.ID, "schema-seg-session", 10)
	if err != nil || len(segs) != 1 || segs[0].Seq != 0 || !segs[0].StartedAt.Equal(now) {
		t.Fatalf("replay segment CRUD rows=%#v err=%v", segs, err)
	}
	if err := store.RecordNotificationDelivery(ctx, channel.ID, rule.ID, "sent", "", 1); err != nil {
		t.Fatal(err)
	}
	if releases, err := store.ListExpiredSourceMapReleases(
		ctx,
		now.Add(time.Second),
		10,
	); err != nil ||
		len(releases) != 1 {
		t.Fatalf("retention query releases=%v err=%v", releases, err)
	}
	readRule, err := store.client.AlertRule.Get(ctx, rule.ID)
	if err != nil || readRule.Labels["team"] != "core" {
		t.Fatalf("JSON round trip rule=%#v err=%v", readRule, err)
	}
	profile, err := store.client.ProfileMeta.Query().Where(profilemeta.BlobKeyEQ("schema-profile")).Only(ctx)
	if err != nil || !profile.Start.Equal(now) {
		t.Fatalf("timestamp round trip profile=%#v err=%v", profile, err)
	}
}
