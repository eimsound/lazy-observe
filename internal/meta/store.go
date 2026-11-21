package meta

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/eimsound/lazy-observe/internal/backoff"
	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/auditevent"
	"github.com/eimsound/lazy-observe/internal/db/ent/project"
	"github.com/eimsound/lazy-observe/internal/db/ent/projectapitoken"
	"github.com/eimsound/lazy-observe/internal/db/ent/session"
	"github.com/eimsound/lazy-observe/internal/db/ent/user"
	"github.com/eimsound/lazy-observe/internal/db/ent/userapitoken"
	"github.com/eimsound/lazy-observe/internal/secret"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql migrations/postgres/*.sql
var migrationFiles embed.FS

type Store struct {
	client             *dbent.Client
	db                 *sql.DB
	dbURL              string
	dialect            string
	startupLockTimeout time.Duration
	issueMu            sync.Mutex
	aiSettingsMu       sync.Mutex
	sourceMapMu        sync.Mutex
	replayMu           sync.Mutex
	projectMu          sync.Mutex
	revision           atomic.Uint64
	secretBox          atomic.Pointer[secret.Box]
}

type ProjectInfo struct {
	ID   int
	Name string
	Slug string
}

type UserInfo struct {
	ID          int        `json:"id"`
	Username    string     `json:"username"`
	Role        string     `json:"role"`
	DisplayName string     `json:"display_name,omitempty"`
	Email       string     `json:"email,omitempty"`
	CreatedAt   time.Time  `json:"created_at,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	// MustChangePassword is set for the generated bootstrap admin password and
	// cleared by the user's own password change.
	MustChangePassword bool `json:"must_change_password"`
}

// ManagedUserCreate contains every account and profile field committed by one
// administrator user-creation operation.
type ManagedUserCreate struct {
	Username    string
	Password    string
	Role        string
	DisplayName string
	Email       string
}

type ManagedUserUpdate struct {
	ID          int
	Username    string
	Password    string
	Role        string
	DisplayName *string
	Email       *string
}

// AccountSession is the persistence input used by the account session adapter.
type AccountSession struct {
	Token     string
	UserID    int
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AccountSessionRef is a credential-free session listing value.
type AccountSessionRef struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
	Current   bool
}

type AuditRecord struct {
	ID            int       `json:"id"`
	Action        string    `json:"action"`
	ActorID       int       `json:"actor_id,omitempty"`
	ActorUsername string    `json:"actor_username"`
	ActorRole     string    `json:"actor_role,omitempty"`
	TargetType    string    `json:"target_type"`
	TargetID      int       `json:"target_id,omitempty"`
	TargetName    string    `json:"target_name"`
	TargetRole    string    `json:"target_role,omitempty"`
	ProjectID     int       `json:"project_id,omitempty"`
	ProjectSlug   string    `json:"project_slug,omitempty"`
	ChangedFields string    `json:"changed_fields,omitempty"`
	Success       bool      `json:"success"`
	FailureReason string    `json:"failure_reason,omitempty"`
	HTTPMethod    string    `json:"http_method"`
	HTTPPath      string    `json:"http_path"`
	CreatedAt     time.Time `json:"created_at"`
}

type AuditListOptions struct {
	Action     string
	Actor      string
	TargetType string
	ProjectID  int
	BeforeID   int
	Limit      int
}

type ProjectAPITokenInfo struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Revoked    bool       `json:"revoked"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type UserAPITokenInfo struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Revoked    bool       `json:"revoked"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

const SelfMonitoringProjectSlug = "lazy-observe"

// ErrInvalidProjectAPIToken distinguishes an unknown or revoked credential from a
// transient metadata-store failure so gateways can return 401 vs 503 safely.
var ErrInvalidProjectAPIToken = errors.New("invalid project API token")

var ErrInvalidUserAPIToken = errors.New("invalid user API token")

// ErrProjectSlugReserved is returned when creating a project with a slug that
// belonged to a previously deleted project. Reusing a slug would expose the
// prior project's retained metrics or uncollected blobs under the same key.
var ErrProjectSlugReserved = errors.New("project slug was previously used and cannot be reused")

// ErrProjectSlugImmutable is returned when an update attempts to change an
// existing project's slug. Slugs serve as non-reusable tenant keys across
// metrics, logs, traces, and blobs.
var ErrProjectSlugImmutable = errors.New("project slug is immutable")

const projectSlugAdvisoryLockNamespace = 7303

const projectAPITokenPrefixLength = 11

const userAPITokenPrefixLength = 12

const apiTokenLastUsedInterval = time.Minute

const (
	DefaultMigrationLockTimeout    = time.Minute
	migrationUnlockTimeout         = 5 * time.Second
	DefaultPostgresMaxOpenConns    = 25
	DefaultPostgresMaxIdleConns    = 10
	DefaultPostgresConnMaxLifetime = 30 * time.Minute
	DefaultPostgresConnMaxIdleTime = 5 * time.Minute
)

// OpenOptions controls metadata-store startup behavior. A zero lock timeout
// selects DefaultMigrationLockTimeout so existing OpenStore callers retain a
// bounded PostgreSQL startup path.
type OpenOptions struct {
	MigrationLockTimeout time.Duration
	ApplicationVersion   string
	SecretBox            *secret.Box
	MaxOpenConns         int
	MaxIdleConns         int
	ConnMaxLifetime      time.Duration
	ConnMaxIdleTime      time.Duration
}

// OpenStore opens a SQLite path/sqlite:// URL or a postgres:// URL. Plain paths
// remain supported so existing callers keep the zero-configuration SQLite path.
func OpenStore(ctx context.Context, dbURL string) (*Store, error) {
	return OpenStoreWithOptions(ctx, dbURL, OpenOptions{})
}

// OpenStoreWithOptions opens the metadata store with explicit startup options.
func OpenStoreWithOptions(ctx context.Context, dbURL string, options OpenOptions) (*Store, error) {
	if options.MigrationLockTimeout == 0 {
		options.MigrationLockTimeout = DefaultMigrationLockTimeout
	}
	if options.MigrationLockTimeout < 0 {
		return nil, errors.New("migration lock timeout must be positive")
	}
	if strings.TrimSpace(options.ApplicationVersion) == "" {
		options.ApplicationVersion = "unknown"
	}
	driverName, entDialect, dsn, sqlitePath, err := parseDBURL(dbURL)
	if err != nil {
		return nil, err
	}
	if entDialect == dialect.SQLite {
		if err := fsValidParent(sqlitePath); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open metadata database: %w", err)
	}
	switch entDialect {
	case dialect.SQLite:
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	case dialect.Postgres:
		configurePostgresPool(db, options)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect metadata database: %w", err)
	}
	if err := runMigrations(ctx, db, entDialect, options); err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{
		client: dbent.NewClient(dbent.Driver(entsql.OpenDB(entDialect, db))), db: db, dbURL: dbURL,
		dialect: entDialect, startupLockTimeout: options.MigrationLockTimeout,
	}
	if options.SecretBox != nil {
		store.secretBox.Store(options.SecretBox)
	}
	return store, nil
}

func configurePostgresPool(db *sql.DB, options OpenOptions) {
	maxOpen := options.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = DefaultPostgresMaxOpenConns
	}
	maxIdle := options.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = DefaultPostgresMaxIdleConns
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	maxLifetime := options.ConnMaxLifetime
	if maxLifetime <= 0 {
		maxLifetime = DefaultPostgresConnMaxLifetime
	}
	maxIdleTime := options.ConnMaxIdleTime
	if maxIdleTime <= 0 {
		maxIdleTime = DefaultPostgresConnMaxIdleTime
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(maxLifetime)
	db.SetConnMaxIdleTime(maxIdleTime)
}

// PostgresDiagnostics summarizes the read-only health and schema state of a
// PostgreSQL metadata database without creating tables or running migrations.
type PostgresDiagnostics struct {
	Initialized            bool
	HasBootstrapAdmin      bool
	PlatformKeyFingerprint string
}

// DiagnosePostgresReadOnly verifies connectivity, baseline schema version and
// checksums, and optional bootstrap metadata against PostgreSQL without
// issuing any DDL, migrations, or write queries.
func DiagnosePostgresReadOnly(
	ctx context.Context,
	dbURL string,
	options OpenOptions,
) (PostgresDiagnostics, error) {
	driverName, entDialect, dsn, _, err := parseDBURL(dbURL)
	if err != nil {
		return PostgresDiagnostics{}, err
	}
	if entDialect != dialect.Postgres {
		return PostgresDiagnostics{}, errors.New("meta.db_url must be a PostgreSQL URL")
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return PostgresDiagnostics{}, fmt.Errorf("open metadata database: %w", err)
	}
	defer db.Close()
	configurePostgresPool(db, options)
	if err := db.PingContext(ctx); err != nil {
		return PostgresDiagnostics{}, fmt.Errorf("connect metadata database: %w", err)
	}

	var relation sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.schema_version')`).Scan(&relation); err != nil {
		return PostgresDiagnostics{}, fmt.Errorf("inspect schema_version table: %w", err)
	}
	if !relation.Valid {
		return PostgresDiagnostics{Initialized: false}, nil
	}

	migrations, err := loadMigrationDefinitions("migrations/postgres")
	if err != nil {
		return PostgresDiagnostics{}, fmt.Errorf("read embedded migrations: %w", err)
	}
	if len(migrations) != 1 || migrations[0].version != 1 {
		return PostgresDiagnostics{}, fmt.Errorf(
			"pre-release database requires exactly one version-1 baseline, got %d files",
			len(migrations),
		)
	}
	var markerVersion sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&markerVersion); err != nil {
		return PostgresDiagnostics{}, fmt.Errorf("read database baseline marker: %w", err)
	}
	if !markerVersion.Valid {
		return PostgresDiagnostics{Initialized: false}, nil
	}
	if markerVersion.Int64 != int64(migrations[0].version) {
		return PostgresDiagnostics{}, fmt.Errorf(
			"metadata database baseline marker %d is incompatible with current pre-release baseline; recreate the database",
			markerVersion.Int64,
		)
	}
	if err := validateMigrationChecksums(ctx, db, migrations); err != nil {
		return PostgresDiagnostics{}, err
	}

	diag := PostgresDiagnostics{Initialized: true}
	var adminCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username = $1`, BootstrapAdminUsername).
		Scan(&adminCount); err != nil {
		return PostgresDiagnostics{}, fmt.Errorf("inspect bootstrap admin: %w", err)
	}
	diag.HasBootstrapAdmin = adminCount > 0

	var fingerprint string
	err = db.QueryRowContext(ctx, `SELECT fingerprint FROM platform_key_fingerprints WHERE id = 1`).
		Scan(&fingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PostgresDiagnostics{}, fmt.Errorf("inspect platform key fingerprint: %w", err)
	}
	diag.PlatformKeyFingerprint = fingerprint
	return diag, nil
}

// SetSecretBox configures the platform encryption box used to seal sensitive
// metadata such as notification channel credentials.
func (s *Store) SetSecretBox(box *secret.Box) {
	s.secretBox.Store(box)
}

func parseDBURL(dbURL string) (driverName, entDialect, dsn, sqlitePath string, err error) {
	switch {
	case strings.HasPrefix(dbURL, "postgres://"), strings.HasPrefix(dbURL, "postgresql://"):
		return "pgx", dialect.Postgres, dbURL, "", nil
	case strings.HasPrefix(dbURL, "sqlite://"):
		raw := strings.TrimPrefix(dbURL, "sqlite://")
		if strings.TrimSpace(raw) == "" {
			return "", "", "", "", errors.New("sqlite metadata URL requires a path")
		}
		dsn, filePath := sqliteDSN(raw)
		return "sqlite", dialect.SQLite, dsn, filePath, nil
	case strings.Contains(dbURL, "://"):
		return "", "", "", "", fmt.Errorf("unsupported metadata database URL %q", dbURL)
	case strings.TrimSpace(dbURL) == "":
		return "", "", "", "", errors.New("metadata database path is empty")
	default:
		dsn, filePath := sqliteDSN(dbURL)
		return "sqlite", dialect.SQLite, dsn, filePath, nil
	}
}

// sqliteDSN embeds connection-level PRAGMAs and UTC time formatting in the
// driver DSN so every newly opened or reconnected SQLite connection enforces
// foreign_keys, busy_timeout, WAL mode, and UTC timestamps consistently.
func sqliteDSN(raw string) (dsn, filePath string) {
	base, rawQuery, hasQuery := strings.Cut(raw, "?")
	values := url.Values{}
	if hasQuery {
		if parsed, err := url.ParseQuery(rawQuery); err == nil {
			values = parsed
		}
	}
	if !values.Has("_busy_timeout") && !values.Has("_timeout") {
		values.Set("_busy_timeout", "5000")
	}
	if !values.Has("_foreign_keys") && !values.Has("_fk") {
		values.Set("_foreign_keys", "1")
	}
	if !values.Has("_journal_mode") && !values.Has("_journal") {
		values.Set("_journal_mode", "WAL")
	}
	if !values.Has("_timezone") {
		values.Set("_timezone", "UTC")
	}
	if !values.Has("_time_format") {
		values.Set("_time_format", "string_no_monotonic")
	}
	filePath = strings.TrimPrefix(base, "file:")
	return base + "?" + values.Encode(), filePath
}

func fsValidParent(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	return s.client.Close()
}

// Health verifies that the metadata database remains reachable. A Store is
// returned only after baseline initialization completes, so its presence also
// marks metadata readiness.
func (s *Store) Health(ctx context.Context) map[string]error {
	return map[string]error{"metadata": s.db.PingContext(ctx)}
}

func (s *Store) Revision() uint64 { return s.revision.Load() }

func (s *Store) RecordNotificationDelivery(
	ctx context.Context,
	channelID, ruleID int,
	status, message string,
	attempts int,
) error {
	_, err := s.db.ExecContext(
		ctx,
		bind(
			s.dialect,
			`INSERT INTO notification_deliveries(channel_id, rule_id, status, error, attempts, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		),
		channelID,
		ruleID,
		status,
		message,
		attempts,
		time.Now().UTC(),
	)
	return err
}

func (s *Store) RecordPlatformNotificationDelivery(
	ctx context.Context,
	channelID int,
	status, message string,
	attempts int,
) error {
	_, err := s.db.ExecContext(
		ctx,
		bind(
			s.dialect,
			`INSERT INTO notification_deliveries(channel_id, rule_id, status, error, attempts, created_at) VALUES (?, NULL, ?, ?, ?, ?)`,
		),
		channelID,
		status,
		message,
		attempts,
		time.Now().UTC(),
	)
	return err
}

func (s *Store) AuthenticateProjectAPIToken(ctx context.Context, token string) (ProjectInfo, error) {
	t, err := s.client.ProjectAPIToken.Query().
		Where(projectapitoken.TokenEQ(storedCredential(token)), projectapitoken.RevokedEQ(false)).
		WithProject().
		Only(ctx)
	if err != nil {
		if IsNotFound(err) {
			return ProjectInfo{}, ErrInvalidProjectAPIToken
		}
		return ProjectInfo{}, err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-apiTokenLastUsedInterval)
	if t.LastUsedAt == nil || t.LastUsedAt.Before(cutoff) {
		// Usage metadata must never turn an otherwise valid token into an API
		// failure. The conditional update also prevents concurrent requests from
		// repeatedly taking the SQLite write lock.
		_, _ = s.client.ProjectAPIToken.Update().
			Where(
				projectapitoken.IDEQ(t.ID),
				projectapitoken.Or(projectapitoken.LastUsedAtIsNil(), projectapitoken.LastUsedAtLT(cutoff)),
			).
			SetLastUsedAt(now).
			Save(ctx)
	}
	p, err := t.Edges.ProjectOrErr()
	if err != nil {
		return ProjectInfo{}, err
	}
	return ProjectInfo{ID: p.ID, Name: p.Name, Slug: p.Slug}, nil
}

func (s *Store) AuthenticateUserAPIToken(ctx context.Context, token string) (UserInfo, error) {
	t, err := s.client.UserAPIToken.Query().
		Where(userapitoken.TokenEQ(storedCredential(token)), userapitoken.RevokedEQ(false)).
		WithUser().
		Only(ctx)
	if err != nil {
		if IsNotFound(err) {
			return UserInfo{}, ErrInvalidUserAPIToken
		}
		return UserInfo{}, err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-apiTokenLastUsedInterval)
	if t.LastUsedAt == nil || t.LastUsedAt.Before(cutoff) {
		_, _ = s.client.UserAPIToken.Update().
			Where(
				userapitoken.IDEQ(t.ID),
				userapitoken.Or(userapitoken.LastUsedAtIsNil(), userapitoken.LastUsedAtLT(cutoff)),
			).
			SetLastUsedAt(now).
			Save(ctx)
	}
	u, err := t.Edges.UserOrErr()
	if err != nil {
		return UserInfo{}, err
	}
	return userInfo(u), nil
}

func hashProjectSlugLock(slug string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(slug))
	return int32(h.Sum32())
}

func (s *Store) CreateProject(ctx context.Context, name, slug string) (*Project, error) {
	if s.dialect != dialect.Postgres {
		s.projectMu.Lock()
		defer s.projectMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create project: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`SELECT pg_advisory_xact_lock($1, $2)`,
			projectSlugAdvisoryLockNamespace,
			hashProjectSlugLock(slug),
		); err != nil {
			return nil, fmt.Errorf("lock project slug: %w", err)
		}
	}
	var reserved int
	if err := tx.QueryRowContext(
		ctx,
		bind(s.dialect, `SELECT COUNT(*) FROM deleted_project_slugs WHERE slug = ?`),
		slug,
	).Scan(&reserved); err != nil {
		return nil, fmt.Errorf("check reserved project slug: %w", err)
	}
	if reserved > 0 {
		return nil, ErrProjectSlugReserved
	}
	var existing int
	if err := tx.QueryRowContext(
		ctx,
		bind(s.dialect, `SELECT COUNT(*) FROM projects WHERE slug = ?`),
		slug,
	).Scan(&existing); err != nil {
		return nil, fmt.Errorf("check existing project slug: %w", err)
	}
	if existing > 0 {
		return nil, fmt.Errorf("project slug %q already exists: %w", slug, errConstraint)
	}
	createdAt := time.Now().UTC().Round(0)
	var id int
	if err := tx.QueryRowContext(
		ctx,
		bind(s.dialect, `INSERT INTO projects(name, slug, created_at) VALUES (?, ?, ?) RETURNING id`),
		name,
		slug,
		createdAt,
	).Scan(&id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.revision.Add(1)
	return &Project{ID: id, Name: name, Slug: slug, CreatedAt: createdAt}, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.client.Project.Query().Order(orderAsc(project.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]*Project, 0, len(rows))
	for _, row := range rows {
		result = append(result, projectFromEnt(row))
	}
	return result, nil
}

func (s *Store) GetProject(ctx context.Context, id int) (*Project, error) {
	row, err := s.client.Project.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return projectFromEnt(row), nil
}

func (s *Store) ProjectBySlug(ctx context.Context, slug string) (ProjectInfo, error) {
	p, err := s.client.Project.Query().Where(project.SlugEQ(slug)).Only(ctx)
	if err != nil {
		return ProjectInfo{}, err
	}
	return ProjectInfo{ID: p.ID, Name: p.Name, Slug: p.Slug}, nil
}

func (s *Store) UpdateProject(ctx context.Context, id int, name, slug string) (*Project, error) {
	existing, err := s.client.Project.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if slug != "" && slug != existing.Slug {
		return nil, ErrProjectSlugImmutable
	}
	b := s.client.Project.UpdateOneID(id)
	if name != "" {
		b.SetName(name)
	}
	p, err := b.Save(ctx)
	if err != nil {
		return nil, err
	}
	s.revision.Add(1)
	return projectFromEnt(p), nil
}

// DeleteProject removes a project and all metadata rows scoped to it in one
// transaction and tombstones its slug so it can never be reused. Blob cleanup
// is triggered immediately by the gateway and retried durably by the janitor.
func (s *Store) DeleteProject(ctx context.Context, id int) error {
	if s.dialect != dialect.Postgres {
		s.projectMu.Lock()
		defer s.projectMu.Unlock()
		s.sourceMapMu.Lock()
		defer s.sourceMapMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var projectSlug string
	if err := tx.QueryRowContext(ctx, bind(s.dialect, `SELECT slug FROM projects WHERE id = ?`), id).
		Scan(&projectSlug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &NotFoundError{project.Label}
		}
		return err
	}
	if s.dialect == dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`SELECT pg_advisory_xact_lock($1, $2)`,
			projectSlugAdvisoryLockNamespace,
			hashProjectSlugLock(projectSlug),
		); err != nil {
			return fmt.Errorf("lock project slug: %w", err)
		}
		if err := s.lockSourceMapProjectTx(ctx, tx, id); err != nil {
			return err
		}
	}
	state, err := s.lockErrorInboxStateTx(ctx, tx)
	if err != nil {
		return err
	}
	var activeItems, activeBytes int64
	if err := tx.QueryRowContext(ctx, bind(s.dialect, `SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0)
		FROM telemetry_inbox WHERE project_id = ? AND status IN ('pending', 'processing')`), id).
		Scan(&activeItems, &activeBytes); err != nil {
		return err
	}
	if activeItems > state.activeItems || activeBytes > state.activeBytes {
		return errors.New("error Inbox capacity counters are inconsistent")
	}
	if activeItems > 0 {
		if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
			active_items = active_items - ?, active_bytes = active_bytes - ?, updated_at = ? WHERE id = 1`),
			activeItems, activeBytes, time.Now().UTC()); err != nil {
			return err
		}
	}

	statements := []string{
		`DELETE FROM notification_outbox WHERE project_id = ?`,
		`DELETE FROM notification_deliveries WHERE rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_rule_leases WHERE rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_rule_states WHERE rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_rule_channels WHERE alert_rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_events WHERE rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_rules WHERE project_id = ?`,
		`DELETE FROM project_alert_config_channels WHERE project_alert_config_id IN (SELECT id FROM project_alert_configs WHERE project_id = ?)`,
		`DELETE FROM project_alert_configs WHERE project_id = ?`,
		`DELETE FROM platform_error_rate_buckets WHERE project_id = ?`,
		`DELETE FROM platform_alert_states WHERE project_id = ?`,
		`DELETE FROM project_api_tokens WHERE project_id = ?`,
		`DELETE FROM source_map_upload_tokens WHERE project_id = ?`,
		`DELETE FROM silences WHERE project_id = ?`,
		`DELETE FROM inhibition_rules WHERE project_id = ?`,
		`DELETE FROM issue_lifecycle_events WHERE project_id = ?`,
		`DELETE FROM telemetry_inbox WHERE project_id = ?`,
		`DELETE FROM issues WHERE project_id = ?`,
		`DELETE FROM replay_segments WHERE project_id = ?`,
		`DELETE FROM replay_sessions WHERE project_id = ?`,
		`DELETE FROM source_map_upload_pins WHERE project_id = ?`,
		`DELETE FROM source_map_artifacts WHERE project_id = ?`,
		`DELETE FROM scrape_targets WHERE project_id = ?`,
		`DELETE FROM profile_metas WHERE project_id = ?`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, bind(s.dialect, statement), id); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, bind(s.dialect, `DELETE FROM projects WHERE id = ?`), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return &NotFoundError{project.Label}
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(
			s.dialect,
			`INSERT INTO deleted_project_slugs(slug, deleted_at, blobs_cleaned_at) VALUES (?, ?, NULL)
			 ON CONFLICT(slug) DO UPDATE SET deleted_at = excluded.deleted_at, blobs_cleaned_at = NULL`,
		),
		projectSlug,
		time.Now().UTC().Round(0),
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.revision.Add(1)
	return nil
}

// ListPendingDeletedProjectBlobs returns slugs of deleted projects whose
// associated blob prefixes have not been marked cleaned yet.
func (s *Store) ListPendingDeletedProjectBlobs(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(
		ctx,
		bind(
			s.dialect,
			`SELECT slug FROM deleted_project_slugs WHERE blobs_cleaned_at IS NULL ORDER BY deleted_at, slug LIMIT ?`,
		),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slugs := make([]string, 0)
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// MarkDeletedProjectBlobsCleaned records that all blob prefixes for a deleted
// project have been reclaimed.
func (s *Store) MarkDeletedProjectBlobsCleaned(ctx context.Context, slug string, cleanedAt time.Time) error {
	_, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, `UPDATE deleted_project_slugs SET blobs_cleaned_at = ? WHERE slug = ?`),
		cleanedAt.UTC().Round(0),
		slug,
	)
	return err
}

func (s *Store) CreateProjectAPIToken(ctx context.Context, projectID int) (string, error) {
	_, value, err := s.CreateNamedProjectAPIToken(ctx, projectID, "default")
	return value, err
}

// CreateNamedProjectAPIToken creates a token whose secret is returned only to the
// caller. Subsequent list operations expose only its non-secret prefix.
func (s *Store) CreateNamedProjectAPIToken(
	ctx context.Context,
	projectID int,
	name string,
) (ProjectAPITokenInfo, string, error) {
	token, err := randomToken()
	if err != nil {
		return ProjectAPITokenInfo{}, "", err
	}
	info, err := s.createProjectAPITokenValue(ctx, projectID, name, token)
	if err != nil {
		return ProjectAPITokenInfo{}, "", err
	}
	return info, token, nil
}

func (s *Store) createProjectAPITokenValue(
	ctx context.Context,
	projectID int,
	name, token string,
) (ProjectAPITokenInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ProjectAPITokenInfo{}, errors.New("project API token name is required")
	}
	prefix := token
	if len(prefix) > projectAPITokenPrefixLength {
		prefix = prefix[:projectAPITokenPrefixLength]
	}
	created, err := s.client.ProjectAPIToken.Create().
		SetToken(storedCredential(token)).
		SetName(name).
		SetPrefix(prefix).
		SetProjectID(projectID).
		Save(ctx)
	if err != nil {
		return ProjectAPITokenInfo{}, err
	}
	s.revision.Add(1)
	return tokenInfo(created), nil
}

// storedCredential is the form in which a bearer credential or session token
// is persisted: its SHA-256 hex digest. Tokens are 192-bit random values (or
// key-derived), so an unsalted fast digest is sufficient, and a copy of the
// metadata database no longer yields usable credentials. Lookups digest the
// presented value and compare digests.
func storedCredential(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) RevokeProjectAPIToken(ctx context.Context, token string) error {
	n, err := s.client.ProjectAPIToken.Update().
		Where(projectapitoken.TokenEQ(storedCredential(token))).
		SetRevoked(true).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{projectapitoken.Label}
	}
	s.revision.Add(1)
	return nil
}

func (s *Store) ListProjectAPITokens(ctx context.Context, projectID int) ([]ProjectAPITokenInfo, error) {
	tokens, err := s.client.ProjectAPIToken.Query().
		Where(projectapitoken.ProjectIDEQ(projectID)).
		Order(orderAsc(projectapitoken.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ProjectAPITokenInfo, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, tokenInfo(token))
	}
	return result, nil
}

func tokenInfo(token *dbent.ProjectAPIToken) ProjectAPITokenInfo {
	return ProjectAPITokenInfo{
		ID:         token.ID,
		Name:       token.Name,
		Prefix:     token.Prefix,
		Revoked:    token.Revoked,
		CreatedAt:  token.CreatedAt,
		LastUsedAt: token.LastUsedAt,
	}
}

func (s *Store) RevokeProjectAPITokenByID(ctx context.Context, projectID, tokenID int) error {
	n, err := s.client.ProjectAPIToken.Update().
		Where(projectapitoken.IDEQ(tokenID), projectapitoken.ProjectIDEQ(projectID)).
		SetRevoked(true).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{projectapitoken.Label}
	}
	s.revision.Add(1)
	return nil
}

func (s *Store) ListUserAPITokens(ctx context.Context, userID int) ([]UserAPITokenInfo, error) {
	tokens, err := s.client.UserAPIToken.Query().
		Where(userapitoken.UserIDEQ(userID)).
		Order(orderAsc(userapitoken.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]UserAPITokenInfo, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, userAPITokenInfo(token))
	}
	return result, nil
}

func (s *Store) CreateUserAPIToken(ctx context.Context, userID int, name string) (UserAPITokenInfo, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return UserAPITokenInfo{}, "", errors.New("user API token name is required")
	}
	token, err := randomOpaqueToken("lou_", 24)
	if err != nil {
		return UserAPITokenInfo{}, "", err
	}
	prefix := token
	if len(prefix) > userAPITokenPrefixLength {
		prefix = prefix[:userAPITokenPrefixLength]
	}
	created, err := s.client.UserAPIToken.Create().
		SetToken(storedCredential(token)).
		SetName(name).
		SetPrefix(prefix).
		SetUserID(userID).
		Save(ctx)
	if err != nil {
		return UserAPITokenInfo{}, "", err
	}
	s.revision.Add(1)
	return userAPITokenInfo(created), token, nil
}

func (s *Store) RevokeUserAPITokenByID(ctx context.Context, userID, tokenID int) error {
	n, err := s.client.UserAPIToken.Update().
		Where(userapitoken.IDEQ(tokenID), userapitoken.UserIDEQ(userID)).
		SetRevoked(true).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return &NotFoundError{userapitoken.Label}
	}
	s.revision.Add(1)
	return nil
}

func userAPITokenInfo(token *dbent.UserAPIToken) UserAPITokenInfo {
	return UserAPITokenInfo{
		ID:         token.ID,
		Name:       token.Name,
		Prefix:     token.Prefix,
		Revoked:    token.Revoked,
		CreatedAt:  token.CreatedAt,
		LastUsedAt: token.LastUsedAt,
	}
}

func (s *Store) CreateAccountSession(ctx context.Context, input AccountSession) error {
	return s.client.Session.Create().
		SetToken(storedCredential(input.Token)).
		SetUserID(input.UserID).
		SetCreatedAt(input.CreatedAt.UTC().Round(0)).
		SetExpiresAt(input.ExpiresAt.UTC().Round(0)).
		Exec(ctx)
}

func (s *Store) AuthenticateAccountSession(ctx context.Context, token string, now time.Time) (int, error) {
	sess, err := s.client.Session.Query().Where(session.TokenEQ(storedCredential(token))).Only(ctx)
	if err != nil {
		return 0, err
	}
	if !sess.ExpiresAt.After(now.UTC()) {
		_ = s.client.Session.DeleteOneID(sess.ID).Exec(ctx)
		return 0, errInvalidCredentials
	}
	return sess.UserID, nil
}

func (s *Store) DeleteAccountSession(ctx context.Context, token string) error {
	_, err := s.client.Session.Delete().Where(session.TokenEQ(storedCredential(token))).Exec(ctx)
	return err
}

func (s *Store) ListAccountSessions(
	ctx context.Context,
	userID int,
	currentToken string,
	now time.Time,
) ([]AccountSessionRef, error) {
	sessions, err := s.client.Session.Query().
		Where(session.UserIDEQ(userID), session.ExpiresAtGT(now.UTC().Round(0))).
		Order(orderDesc(session.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]AccountSessionRef, 0, len(sessions))
	for _, sess := range sessions {
		result = append(result, AccountSessionRef{
			ID: storedSessionReference(sess.Token), CreatedAt: sess.CreatedAt, ExpiresAt: sess.ExpiresAt,
			Current: currentToken != "" && sess.Token == storedCredential(currentToken),
		})
	}
	return result, nil
}

func (s *Store) DeleteAccountSessionByRef(ctx context.Context, userID int, reference string) error {
	sessions, err := s.client.Session.Query().Where(session.UserIDEQ(userID)).All(ctx)
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		if storedSessionReference(sess.Token) == reference {
			return s.client.Session.DeleteOneID(sess.ID).Exec(ctx)
		}
	}
	return &NotFoundError{Label: "session"}
}

// accountSessionReference is the public, non-secret ID of a session: the first
// 48 bits of its digest. storedSessionReference derives the same ID from the
// stored digest.
func accountSessionReference(token string) string {
	return storedSessionReference(storedCredential(token))
}

func storedSessionReference(stored string) string { return stored[:12] }

// MarkLogin stamps last_login_at for a successfully authenticated user.
func (s *Store) MarkLogin(ctx context.Context, userID int) error {
	return s.client.User.UpdateOneID(userID).SetLastLoginAt(time.Now().UTC()).Exec(ctx)
}

func (s *Store) AuthenticateCredentials(ctx context.Context, username, password string) (UserInfo, error) {
	u, err := s.client.User.Query().Where(user.UsernameEQ(username)).Only(ctx)
	if err != nil {
		if IsNotFound(err) {
			// Perform equivalent expensive work even when the username does not exist.
			if _, dummyErr := argon2DummyContext(ctx, password); dummyErr != nil {
				return UserInfo{}, dummyErr
			}
			return UserInfo{}, errInvalidCredentials
		}
		return UserInfo{}, err
	}
	ok, err := VerifyPasswordContext(ctx, u.PasswordHash, password)
	if err != nil {
		return UserInfo{}, err
	}
	if !ok {
		return UserInfo{}, errInvalidCredentials
	}
	return userInfo(u), nil
}

func (s *Store) ListUsers(ctx context.Context) ([]UserInfo, error) {
	users, err := s.client.User.Query().Order(orderAsc(user.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]UserInfo, 0, len(users))
	for _, u := range users {
		result = append(result, userInfo(u))
	}
	return result, nil
}

func (s *Store) GetUser(ctx context.Context, id int) (UserInfo, error) {
	u, err := s.client.User.Get(ctx, id)
	if err != nil {
		return UserInfo{}, err
	}
	return userInfo(u), nil
}

// UserByUsername resolves one account by name. IsNotFound reports an unknown
// user, which operator-facing tools turn into a clear diagnostic.
func (s *Store) UserByUsername(ctx context.Context, username string) (UserInfo, error) {
	u, err := s.client.User.Query().Where(user.UsernameEQ(username)).Only(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	return userInfo(u), nil
}

func (s *Store) CreateUser(ctx context.Context, username, password, role string) (UserInfo, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return UserInfo{}, err
	}
	u, err := s.client.User.Create().SetUsername(username).SetPasswordHash(hash).SetRole(user.Role(role)).Save(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	return userInfo(u), nil
}

// CreateManagedUser atomically creates all account and profile fields for an
// administrator-managed user.
func (s *Store) CreateManagedUser(ctx context.Context, input ManagedUserCreate) (UserInfo, error) {
	hash, err := HashPassword(input.Password)
	if err != nil {
		return UserInfo{}, err
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	defer func() { _ = tx.Rollback() }()

	builder := tx.User.Create().
		SetUsername(input.Username).
		SetPasswordHash(hash).
		SetRole(user.Role(input.Role))
	if input.DisplayName != "" {
		builder.SetDisplayName(input.DisplayName)
	}
	if input.Email != "" {
		builder.SetEmail(input.Email)
	}
	created, err := builder.Save(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	result := userInfo(created)
	if err := tx.Commit(); err != nil {
		return UserInfo{}, err
	}
	return result, nil
}

func (s *Store) UpdateUser(ctx context.Context, id int, username, password, role string) (UserInfo, error) {
	b := s.client.User.UpdateOneID(id)
	if username != "" {
		b.SetUsername(username)
	}
	if password != "" {
		hash, err := HashPassword(password)
		if err != nil {
			return UserInfo{}, err
		}
		b.SetPasswordHash(hash)
	}
	if role != "" {
		b.SetRole(user.Role(role))
	}
	u, err := b.Save(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	return userInfo(u), nil
}

// UpdateManagedUser atomically updates all requested account and profile
// fields for one administrator-managed user, protects the last admin from
// demotion, and revokes persisted sessions on password reset or role downgrade.
func (s *Store) UpdateManagedUser(ctx context.Context, input ManagedUserUpdate) (result UserInfo, err error) {
	var hash string
	if input.Password != "" {
		hash, err = HashPasswordContext(ctx, input.Password)
		if err != nil {
			return UserInfo{}, err
		}
	}
	if input.Role != "" && input.Role != string(user.RoleAdmin) {
		release, lockErr := s.acquireAdminRoleCoordination(ctx)
		if lockErr != nil {
			return UserInfo{}, lockErr
		}
		defer func() { err = errors.Join(err, release()) }()
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := tx.User.Get(ctx, input.ID)
	if err != nil {
		return UserInfo{}, err
	}
	demotingAdmin := existing.Role == user.RoleAdmin && input.Role != "" && input.Role != string(user.RoleAdmin)
	if demotingAdmin {
		if err := ensureNotLastAdminTx(ctx, tx); err != nil {
			return UserInfo{}, err
		}
	}

	builder := tx.User.UpdateOneID(input.ID)
	if input.Username != "" {
		builder.SetUsername(input.Username)
	}
	if hash != "" {
		builder.SetPasswordHash(hash)
	}
	if input.Role != "" {
		builder.SetRole(user.Role(input.Role))
	}
	if input.DisplayName != nil {
		if *input.DisplayName == "" {
			builder.ClearDisplayName()
		} else {
			builder.SetDisplayName(*input.DisplayName)
		}
	}
	if input.Email != nil {
		if *input.Email == "" {
			builder.ClearEmail()
		} else {
			builder.SetEmail(*input.Email)
		}
	}
	updated, err := builder.Save(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	if hash != "" || isUserRoleDowngrade(existing.Role, input.Role) {
		if _, err := tx.Session.Delete().Where(session.UserIDEQ(input.ID)).Exec(ctx); err != nil {
			return UserInfo{}, err
		}
	}
	result = userInfo(updated)
	if err := tx.Commit(); err != nil {
		return UserInfo{}, err
	}
	return result, nil
}

func (s *Store) DeleteUser(ctx context.Context, id int) (err error) {
	release, lockErr := s.acquireAdminRoleCoordination(ctx)
	if lockErr != nil {
		return lockErr
	}
	defer func() { err = errors.Join(err, release()) }()

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := tx.User.Get(ctx, id)
	if err != nil {
		return err
	}
	if existing.Role == user.RoleAdmin {
		if err := ensureNotLastAdminTx(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.Session.Delete().Where(session.UserIDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	if err := tx.User.DeleteOneID(id).Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// ChangePassword verifies the current password and stores the new hash.
// A wrong current password returns errInvalidCredentials.
func (s *Store) ChangePassword(ctx context.Context, userID int, currentPassword, newPassword string) error {
	u, err := s.client.User.Get(ctx, userID)
	if err != nil {
		return err
	}
	ok, err := VerifyPasswordContext(ctx, u.PasswordHash, currentPassword)
	if err != nil {
		return err
	}
	if !ok {
		return errInvalidCredentials
	}
	hash, err := HashPasswordContext(ctx, newPassword)
	if err != nil {
		return err
	}
	return s.client.User.UpdateOneID(userID).SetPasswordHash(hash).SetMustChangePassword(false).Exec(ctx)
}

// SetPasswordHash stores an already hashed password and sets the forced-change
// flag explicitly. It backs operator-facing resets such as `lazy-observe admin
// reset-password`: those callers hash first, revoke sessions next, and only then
// write the hash, so they cannot use a password-accepting helper.
func (s *Store) SetPasswordHash(ctx context.Context, userID int, hash string, mustChangePassword bool) error {
	return s.client.User.UpdateOneID(userID).
		SetPasswordHash(hash).
		SetMustChangePassword(mustChangePassword).
		Exec(ctx)
}

// UpdateUserProfile updates display name and email. A non-nil pointer sets
// the field, clearing it when the value is empty; nil leaves it unchanged.
func (s *Store) UpdateUserProfile(ctx context.Context, id int, displayName, email *string) (UserInfo, error) {
	b := s.client.User.UpdateOneID(id)
	if displayName != nil {
		if *displayName == "" {
			b.ClearDisplayName()
		} else {
			b.SetDisplayName(*displayName)
		}
	}
	if email != nil {
		if *email == "" {
			b.ClearEmail()
		} else {
			b.SetEmail(*email)
		}
	}
	u, err := b.Save(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	return userInfo(u), nil
}

// DeleteOtherAccountSessions removes every session of the user except keepToken.
func (s *Store) DeleteOtherAccountSessions(ctx context.Context, userID int, keepToken string) error {
	_, err := s.client.Session.Delete().
		Where(session.UserIDEQ(userID), session.TokenNEQ(storedCredential(keepToken))).
		Exec(ctx)
	return err
}

func (s *Store) RecordAudit(ctx context.Context, record AuditRecord) error {
	b := s.client.AuditEvent.Create().
		SetAction(record.Action).
		SetActorUsername(record.ActorUsername).
		SetTargetType(record.TargetType).
		SetTargetName(record.TargetName).
		SetTargetRole(record.TargetRole).
		SetChangedFields(record.ChangedFields).
		SetSuccess(record.Success).
		SetFailureReason(record.FailureReason).
		SetHTTPMethod(record.HTTPMethod).
		SetHTTPPath(record.HTTPPath)
	if record.ActorID > 0 {
		b.SetActorID(record.ActorID)
	}
	if record.ActorRole != "" {
		b.SetActorRole(record.ActorRole)
	}
	if record.TargetID > 0 {
		b.SetTargetID(record.TargetID)
	}
	if record.ProjectID > 0 {
		b.SetProjectID(record.ProjectID)
	}
	if record.ProjectSlug != "" {
		b.SetProjectSlug(record.ProjectSlug)
	}
	return b.Exec(ctx)
}

func (s *Store) ListAudit(ctx context.Context, options AuditListOptions) ([]AuditRecord, error) {
	limit := options.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	q := s.client.AuditEvent.Query()
	if options.BeforeID > 0 {
		q = q.Where(auditevent.IDLT(options.BeforeID))
	}
	if options.Action != "" {
		q = q.Where(auditevent.ActionEQ(options.Action))
	}
	if options.Actor != "" {
		q = q.Where(auditevent.ActorUsernameEQ(options.Actor))
	}
	if options.TargetType != "" {
		q = q.Where(auditevent.TargetTypeEQ(options.TargetType))
	}
	if options.ProjectID > 0 {
		q = q.Where(auditevent.ProjectIDEQ(options.ProjectID))
	}
	events, err := q.Order(orderDesc(auditevent.FieldID)).Limit(limit).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]AuditRecord, 0, len(events))
	for _, event := range events {
		result = append(result, auditRecord(event))
	}
	return result, nil
}

// DeleteAuditBefore removes audit rows older than cutoff in bounded batches
// and returns the number of deleted rows.
func (s *Store) DeleteAuditBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return s.deleteInBatches(
		ctx,
		"expired audit events",
		`DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events WHERE created_at < ? ORDER BY id LIMIT ?)`,
		cutoff.UTC().Round(0),
	)
}

func auditRecord(event *dbent.AuditEvent) AuditRecord {
	record := AuditRecord{
		ID:            event.ID,
		Action:        event.Action,
		ActorID:       event.ActorID,
		ActorUsername: event.ActorUsername,
		ActorRole:     event.ActorRole,
		TargetType:    event.TargetType,
		TargetID:      event.TargetID,
		TargetName:    event.TargetName,
		TargetRole:    event.TargetRole,
		ProjectID:     event.ProjectID,
		ProjectSlug:   event.ProjectSlug,
		ChangedFields: event.ChangedFields,
		Success:       event.Success,
		FailureReason: event.FailureReason,
		HTTPMethod:    event.HTTPMethod,
		HTTPPath:      event.HTTPPath,
		CreatedAt:     event.CreatedAt,
	}
	return record
}

func (s *Store) acquireBootstrapCoordination(ctx context.Context) (func() error, error) {
	if s.dialect != dialect.Postgres {
		return func() error { return nil }, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve PostgreSQL bootstrap connection: %w", err)
	}
	lockKey, err := acquirePostgresAdvisoryLock(
		ctx,
		conn,
		s.startupLockTimeout,
		"metadata-bootstrap",
		"PostgreSQL bootstrap",
	)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return func() error {
		defer conn.Close()
		return releasePostgresAdvisoryLock(conn, lockKey, "PostgreSQL bootstrap")
	}, nil
}

// IsClustered reports whether this store uses a shared multi-replica backend
// (PostgreSQL) that needs cross-replica coordination.
func (s *Store) IsClustered() bool {
	return s.dialect == dialect.Postgres
}

// TryAcquireJanitorLock attempts to acquire a non-blocking cluster-wide
// advisory lock so only one HA replica runs a retention janitor cycle at a time.
func (s *Store) TryAcquireJanitorLock(ctx context.Context) (func() error, bool, error) {
	return s.tryAcquireSessionAdvisoryLock(ctx, "janitor-retention", "PostgreSQL janitor")
}

// TryAcquireScrapeLeader attempts to acquire a non-blocking cluster-wide
// advisory lock designating the single HA replica responsible for active
// vmagent target scraping.
func (s *Store) TryAcquireScrapeLeader(ctx context.Context) (func() error, bool, error) {
	return s.tryAcquireSessionAdvisoryLock(ctx, "scrape-leader", "PostgreSQL scrape leader")
}

func (s *Store) tryAcquireSessionAdvisoryLock(
	ctx context.Context,
	namespace, operation string,
) (func() error, bool, error) {
	if s.dialect != dialect.Postgres {
		return func() error { return nil }, true, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("reserve %s connection: %w", operation, err)
	}
	lockKey, err := postgresAdvisoryLockKey(ctx, conn, namespace)
	if err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("acquire %s lock: %w", operation, err)
	}
	if !acquired {
		_ = conn.Close()
		return nil, false, nil
	}
	var releaseOnce sync.Once
	var releaseErr error
	return func() error {
		releaseOnce.Do(func() {
			defer conn.Close()
			releaseErr = releasePostgresAdvisoryLock(conn, lockKey, operation)
		})
		return releaseErr
	}, true, nil
}

// Bootstrap idempotently creates the first admin and the default project. The
// admin password never reaches the logs: it comes from admin.PasswordFile, or
// is generated and written to admin.GeneratedPasswordFile (mode 0600). An
// existing admin is left untouched. The default project API token is
// ingest-only (ADR 0010) and is still logged once so first ingest works.
func (s *Store) Bootstrap(
	ctx context.Context,
	logger *slog.Logger,
	admin BootstrapAdmin,
) (token string, err error) {
	if err := admin.validate(); err != nil {
		return "", err
	}
	release, err := s.acquireBootstrapCoordination(ctx)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, release()) }()
	if err := s.ensureBootstrapAdmin(ctx, logger, admin); err != nil {
		return "", err
	}
	_, err = s.ProjectBySlug(ctx, "default")
	if IsNotFound(err) {
		p, err := s.CreateProject(ctx, "Default", "default")
		if err != nil {
			return "", fmt.Errorf("create default project: %w", err)
		}
		token, err = s.CreateProjectAPIToken(ctx, p.ID)
		if err != nil {
			return "", fmt.Errorf("create default project API token: %w", err)
		}
		logger.Info("created default project", "project_id", p.ID, "slug", p.Slug, "project_api_token", token)
		return token, nil
	}
	if err != nil {
		return "", fmt.Errorf("find default project: %w", err)
	}
	return "", nil
}

// BootstrapSelfMonitoring idempotently creates the built-in project and makes
// sure the caller-derived internal project API token is active. The caller
// derives the token from the platform key (secret.Box.Derive), so replicas and
// restarts converge on the same credential without ever reading it back from
// the database. The token is deliberately never logged.
func (s *Store) BootstrapSelfMonitoring(
	ctx context.Context,
	derived string,
) (info ProjectInfo, value string, err error) {
	if !strings.HasPrefix(derived, "lo_") || len(derived) < 32 {
		return ProjectInfo{}, "", errors.New("self-monitoring token must be a derived lo_ token")
	}
	release, err := s.acquireBootstrapCoordination(ctx)
	if err != nil {
		return ProjectInfo{}, "", err
	}
	defer func() { err = errors.Join(err, release()) }()
	info, err = s.ProjectBySlug(ctx, SelfMonitoringProjectSlug)
	if IsNotFound(err) {
		created, createErr := s.CreateProject(ctx, "lazy-observe", SelfMonitoringProjectSlug)
		if createErr != nil {
			return ProjectInfo{}, "", fmt.Errorf("ensure self-monitoring project: %w", createErr)
		}
		info = ProjectInfo{ID: created.ID, Name: created.Name, Slug: created.Slug}
	} else if err != nil {
		return ProjectInfo{}, "", fmt.Errorf("ensure self-monitoring project: %w", err)
	}
	existing, err := s.client.ProjectAPIToken.Query().
		Where(projectapitoken.TokenEQ(storedCredential(derived))).
		Only(ctx)
	switch {
	case err == nil:
		if existing.ProjectID != info.ID {
			return ProjectInfo{}, "", errors.New("self-monitoring token belongs to another project")
		}
		if existing.Revoked {
			if err := existing.Update().SetRevoked(false).Exec(ctx); err != nil {
				return ProjectInfo{}, "", fmt.Errorf("reactivate self-monitoring project API token: %w", err)
			}
			s.revision.Add(1)
		}
		return info, derived, nil
	case !IsNotFound(err):
		return ProjectInfo{}, "", fmt.Errorf("find self-monitoring project API token: %w", err)
	}
	if _, err := s.createProjectAPITokenValue(ctx, info.ID, "self-monitoring", derived); err != nil {
		return ProjectInfo{}, "", fmt.Errorf("create self-monitoring project API token: %w", err)
	}
	return info, derived, nil
}

func (s *Store) ensureBootstrapAdmin(ctx context.Context, logger *slog.Logger, admin BootstrapAdmin) error {
	exists, err := s.client.User.Query().Exist(ctx)
	if err != nil {
		return fmt.Errorf("find existing users: %w", err)
	}
	if exists {
		return nil
	}
	host := bootstrapHost()
	if admin.PasswordFile != "" {
		password, err := ReadAdminPasswordFile(admin.PasswordFile)
		if err != nil {
			return fmt.Errorf("create bootstrap admin: %w", err)
		}
		// The deployer chose this password, so it is not forced to change.
		if err := s.createBootstrapAdmin(ctx, password, false); err != nil {
			return fmt.Errorf("create bootstrap admin: %w", err)
		}
		logger.Info(
			"created bootstrap admin",
			"username", BootstrapAdminUsername,
			"password_source", "admin_password_file",
			"password_file", admin.PasswordFile,
			"host", host,
		)
		return nil
	}
	password, err := randomPassword()
	if err != nil {
		return err
	}
	// Persist before creating the account: a crash in between leaves a stale
	// file that the next start overwrites, never an admin nobody can log in as.
	if err := writeInitialAdminPassword(admin.GeneratedPasswordFile, password); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	// The generated password sits in a file on disk, so the first login must
	// replace it before anything else is allowed.
	if err := s.createBootstrapAdmin(ctx, password, true); err != nil {
		_ = os.Remove(admin.GeneratedPasswordFile)
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	logger.Info(
		"created bootstrap admin",
		"username", BootstrapAdminUsername,
		"password_source", "generated",
		"host", host,
	)
	logger.Info(
		"initial admin password written",
		"username", BootstrapAdminUsername,
		"path", admin.GeneratedPasswordFile,
		"host", host,
	)
	return nil
}

func (s *Store) createBootstrapAdmin(ctx context.Context, password string, mustChangePassword bool) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.client.User.Create().
		SetUsername(BootstrapAdminUsername).
		SetPasswordHash(hash).
		SetRole(user.RoleAdmin).
		SetMustChangePassword(mustChangePassword).
		Exec(ctx)
}

func randomToken() (string, error) {
	return randomOpaqueToken("lo_", 24)
}

func randomOpaqueToken(prefix string, size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate admin password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func userInfo(u *dbent.User) UserInfo {
	info := UserInfo{
		ID: u.ID, Username: u.Username, Role: string(u.Role), CreatedAt: u.CreatedAt,
		MustChangePassword: u.MustChangePassword,
	}
	if u.DisplayName != "" {
		info.DisplayName = u.DisplayName
	}
	if u.Email != "" {
		info.Email = u.Email
	}
	if !u.LastLoginAt.IsZero() {
		lastLogin := u.LastLoginAt
		info.LastLoginAt = &lastLogin
	}
	return info
}

type migrationExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func runMigrations(ctx context.Context, db *sql.DB, dbDialect string, options OpenOptions) (err error) {
	if dbDialect != dialect.Postgres {
		return runMigrationSequence(ctx, db, dbDialect, options.ApplicationVersion)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve PostgreSQL migration connection: %w", err)
	}
	defer conn.Close()
	lockKey, err := acquirePostgresMigrationLock(ctx, conn, options.MigrationLockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := releasePostgresMigrationLock(conn, lockKey); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	return runMigrationSequence(ctx, conn, dbDialect, options.ApplicationVersion)
}

func acquirePostgresMigrationLock(ctx context.Context, conn *sql.Conn, timeout time.Duration) (int64, error) {
	return acquirePostgresAdvisoryLock(ctx, conn, timeout, "metadata-migrations", "PostgreSQL migration")
}

func acquirePostgresAdvisoryLock(
	ctx context.Context,
	conn *sql.Conn,
	timeout time.Duration,
	namespace, operation string,
) (int64, error) {
	lockKey, err := postgresAdvisoryLockKey(ctx, conn, namespace)
	if err != nil {
		return 0, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	retryIndex := 0
	for {
		var acquired bool
		if err := conn.QueryRowContext(waitCtx, `SELECT pg_try_advisory_lock($1)`, lockKey).
			Scan(&acquired); err != nil {
			if waitCtx.Err() != nil {
				if ctx.Err() != nil {
					return 0, fmt.Errorf("acquire %s lock: %w", operation, ctx.Err())
				}
				return 0, fmt.Errorf("timed out acquiring %s lock after %s: %w", operation, timeout, waitCtx.Err())
			}
			return 0, fmt.Errorf("acquire %s lock: %w", operation, err)
		}
		if acquired {
			return lockKey, nil
		}
		delay, delayErr := backoff.Delay(retryIndex, 25*time.Millisecond, time.Second, 1, 0, 0)
		if delayErr != nil {
			return 0, fmt.Errorf("calculate %s lock retry: %w", operation, delayErr)
		}
		timer := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return 0, fmt.Errorf("acquire %s lock: %w", operation, ctx.Err())
			}
			return 0, fmt.Errorf("timed out acquiring %s lock after %s: %w", operation, timeout, waitCtx.Err())
		case <-timer.C:
		}
		retryIndex++
	}
}

func postgresMigrationLockKey(ctx context.Context, conn *sql.Conn) (int64, error) {
	return postgresAdvisoryLockKey(ctx, conn, "metadata-migrations")
}

func postgresAdvisoryLockKey(ctx context.Context, conn *sql.Conn, namespace string) (int64, error) {
	var databaseName string
	if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
		return 0, fmt.Errorf("read PostgreSQL database name for %s lock: %w", namespace, err)
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("lazy-observe:" + namespace + ":" + databaseName))
	return int64(hash.Sum64()), nil
}

func releasePostgresMigrationLock(conn *sql.Conn, lockKey int64) error {
	return releasePostgresAdvisoryLock(conn, lockKey, "PostgreSQL migration")
}

func releasePostgresAdvisoryLock(conn *sql.Conn, lockKey int64, operation string) error {
	return releasePostgresAdvisoryLockQuery(conn, operation, `SELECT pg_advisory_unlock($1)`, lockKey)
}

func releasePostgresAdvisoryLock2(conn *sql.Conn, namespace, key int, operation string) error {
	return releasePostgresAdvisoryLockQuery(conn, operation, `SELECT pg_advisory_unlock($1, $2)`, namespace, key)
}

func releasePostgresAdvisoryLockQuery(conn *sql.Conn, operation, query string, args ...any) error {
	ctx, cancel := context.WithTimeout(context.Background(), migrationUnlockTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRowContext(ctx, query, args...).Scan(&released)
	if err == nil && released {
		return nil
	}
	discardPostgresAdvisoryLockConn(conn)
	if err != nil {
		return fmt.Errorf("release %s lock: %w", operation, err)
	}
	return fmt.Errorf("release %s lock: lock was not held by the reserved connection", operation)
}

func discardPostgresAdvisoryLockConn(conn *sql.Conn) {
	// A failed unlock must never return a session-scoped lock to database/sql's
	// pool. pgx stdlib exposes the underlying connection only for this discard.
	_ = conn.Raw(func(driverConn any) error {
		if pgxConn, ok := driverConn.(*pgxstdlib.Conn); ok {
			closeCtx, cancel := context.WithTimeout(context.Background(), migrationUnlockTimeout)
			defer cancel()
			_ = pgxConn.Conn().Close(closeCtx)
		}
		return driver.ErrBadConn
	})
}

type migrationDefinition struct {
	version  int
	name     string
	script   []byte
	checksum string
}

func runMigrationSequence(ctx context.Context, db migrationExecutor, dbDialect, applicationVersion string) error {
	schemaVersionDDL := `CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		app_version TEXT NOT NULL,
		applied_at TIMESTAMP NOT NULL,
		duration_ms INTEGER NOT NULL
	)`
	if dbDialect == dialect.Postgres {
		schemaVersionDDL = `CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			checksum TEXT NOT NULL,
			app_version TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL,
			duration_ms BIGINT NOT NULL
		)`
	}
	if _, err := db.ExecContext(ctx, schemaVersionDDL); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	directory := "migrations"
	if dbDialect == dialect.Postgres {
		directory = "migrations/postgres"
	}
	migrations, err := loadMigrationDefinitions(directory)
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	if len(migrations) == 0 {
		return errors.New("embedded migration set is empty")
	}
	if len(migrations) != 1 || migrations[0].version != 1 {
		return fmt.Errorf("pre-release database requires exactly one version-1 baseline, got %d files", len(migrations))
	}
	baselineVersion := migrations[0].version
	var markerVersion sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&markerVersion); err != nil {
		return fmt.Errorf("read database baseline marker: %w", err)
	}
	if markerVersion.Valid && markerVersion.Int64 != int64(baselineVersion) {
		return fmt.Errorf(
			"metadata database baseline marker %d is incompatible with current pre-release baseline; recreate the database",
			markerVersion.Int64,
		)
	}
	if err := validateMigrationChecksums(ctx, db, migrations); err != nil {
		return err
	}
	for _, migration := range migrations {
		var applied int
		if err := db.QueryRowContext(ctx, bind(dbDialect, `SELECT COUNT(*) FROM schema_version WHERE version = ?`), migration.version).
			Scan(&applied); err != nil {
			return fmt.Errorf("read migration version %d: %w", migration.version, err)
		}
		if applied != 0 {
			continue
		}
		started := time.Now()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx, string(migration.script)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", migration.version, err)
		}
		durationMillis := time.Since(started).Milliseconds()
		if _, err := tx.ExecContext(
			ctx,
			bind(
				dbDialect,
				`INSERT INTO schema_version(version, name, checksum, app_version, applied_at, duration_ms) VALUES (?, ?, ?, ?, ?, ?)`,
			),
			migration.version,
			migration.name,
			migration.checksum,
			applicationVersion,
			time.Now().UTC(),
			durationMillis,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.version, err)
		}
	}
	return nil
}

func loadMigrationDefinitions(directory string) ([]migrationDefinition, error) {
	entries, err := migrationEntries(migrationFiles, directory)
	if err != nil {
		return nil, err
	}
	definitions := make([]migrationDefinition, 0, len(entries))
	for _, entry := range entries {
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("invalid migration name %q", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("invalid migration version %q: %w", prefix, err)
		}
		script, err := migrationFiles.ReadFile(directory + "/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		checksum := sha256.Sum256(script)
		definitions = append(definitions, migrationDefinition{
			version: version, name: entry.Name(), script: script, checksum: hex.EncodeToString(checksum[:]),
		})
	}
	return definitions, nil
}

func validateMigrationChecksums(ctx context.Context, db migrationExecutor, migrations []migrationDefinition) error {
	expected := make(map[int]string, len(migrations))
	for _, migration := range migrations {
		expected[migration.version] = migration.checksum
	}
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_version ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read migration checksums: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int
		var checksum sql.NullString
		if err := rows.Scan(&version, &checksum); err != nil {
			return fmt.Errorf("scan migration checksum: %w", err)
		}
		if checksum.Valid && checksum.String != expected[version] {
			return fmt.Errorf(
				"metadata migration %d checksum mismatch: database=%s embedded=%s",
				version,
				checksum.String,
				expected[version],
			)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration checksums: %w", err)
	}
	return nil
}

func migrationEntries(fsys fs.FS, directory string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(fsys, directory)
	if err != nil {
		return nil, err
	}
	result := entries[:0]
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "._") || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name() < result[j].Name() })
	return result, nil
}

func bind(dbDialect, query string) string {
	if dbDialect != dialect.Postgres {
		return query
	}
	var b strings.Builder
	parameter := 1
	for _, r := range query {
		if r == '?' {
			fmt.Fprintf(&b, "$%d", parameter)
			parameter++
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
