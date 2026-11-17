package meta

import (
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/profilemeta"
)

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
		t.Fatalf("migrated table/column/nullability shape differs:\nsqlite: %v\npostgres: %v", want, got)
	}
	assertSchemaCRUD(t, sqlite)
	assertSchemaCRUD(t, postgres)
}

func schemaColumns(t *testing.T, store *Store) []string {
	t.Helper()
	ctx := context.Background()
	var result []string
	if store.dialect == "postgres" {
		rows, err := store.db.QueryContext(ctx, `
			SELECT table_name, column_name, is_nullable
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name <> 'schema_version'
			ORDER BY table_name, ordinal_position`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var table, column, nullable string
			if err := rows.Scan(&table, &column, &nullable); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s.%s:%t", table, column, nullable == "YES"))
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
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s.%s:%t", table, name, notNull == 0 && pk == 0))
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
