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
	revision           atomic.Uint64
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

const projectAPITokenPrefixLength = 11

const userAPITokenPrefixLength = 12

const apiTokenLastUsedInterval = time.Minute

const (
	DefaultMigrationLockTimeout = time.Minute
	migrationUnlockTimeout      = 5 * time.Second
)

// OpenOptions controls metadata-store startup behavior. A zero lock timeout
// selects DefaultMigrationLockTimeout so existing OpenStore callers retain a
// bounded PostgreSQL startup path.
type OpenOptions struct {
	MigrationLockTimeout time.Duration
	ApplicationVersion   string
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
	driverName, entDialect, dsn, err := parseDBURL(dbURL)
	if err != nil {
		return nil, err
	}
	if entDialect == dialect.SQLite {
		if err := fsValidParent(dsn); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open metadata database: %w", err)
	}
	if entDialect == dialect.SQLite {
		db.SetMaxOpenConns(1)
		for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000"} {
			if _, err := db.ExecContext(ctx, pragma); err != nil {
				db.Close()
				return nil, fmt.Errorf("configure metadata database: %w", err)
			}
		}
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect metadata database: %w", err)
	}
	if err := runMigrations(ctx, db, entDialect, options); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{
		client: dbent.NewClient(dbent.Driver(entsql.OpenDB(entDialect, db))), db: db, dbURL: dbURL,
		dialect: entDialect, startupLockTimeout: options.MigrationLockTimeout,
	}, nil
}

func parseDBURL(dbURL string) (driverName, entDialect, dsn string, err error) {
	switch {
	case strings.HasPrefix(dbURL, "postgres://"), strings.HasPrefix(dbURL, "postgresql://"):
		return "pgx", dialect.Postgres, dbURL, nil
	case strings.HasPrefix(dbURL, "sqlite://"):
		path := strings.TrimPrefix(dbURL, "sqlite://")
		if path == "" {
			return "", "", "", errors.New("sqlite metadata URL requires a path")
		}
		return "sqlite", dialect.SQLite, path, nil
	case strings.Contains(dbURL, "://"):
		return "", "", "", fmt.Errorf("unsupported metadata database URL %q", dbURL)
	case strings.TrimSpace(dbURL) == "":
		return "", "", "", errors.New("metadata database path is empty")
	default:
		return "sqlite", dialect.SQLite, dbURL, nil
	}
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

func (s *Store) CreateProject(ctx context.Context, name, slug string) (*Project, error) {
	p, err := s.client.Project.Create().SetName(name).SetSlug(slug).Save(ctx)
	if err == nil {
		s.revision.Add(1)
	}
	return p, err
}

func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	return s.client.Project.Query().Order(orderAsc(project.FieldID)).All(ctx)
}

func (s *Store) GetProject(ctx context.Context, id int) (*Project, error) {
	return s.client.Project.Get(ctx, id)
}

func (s *Store) ProjectBySlug(ctx context.Context, slug string) (ProjectInfo, error) {
	p, err := s.client.Project.Query().Where(project.SlugEQ(slug)).Only(ctx)
	if err != nil {
		return ProjectInfo{}, err
	}
	return ProjectInfo{ID: p.ID, Name: p.Name, Slug: p.Slug}, nil
}

func (s *Store) UpdateProject(ctx context.Context, id int, name, slug string) (*Project, error) {
	b := s.client.Project.UpdateOneID(id)
	if name != "" {
		b.SetName(name)
	}
	if slug != "" {
		b.SetSlug(slug)
	}
	p, err := b.Save(ctx)
	if err == nil {
		s.revision.Add(1)
	}
	return p, err
}

// DeleteProject removes a project and all metadata rows scoped to it in one
// transaction. Blob retention is handled independently by the storage layer;
// deleting a project must not leave relational rows that prevent the CRUD
// operation from completing.
func (s *Store) DeleteProject(ctx context.Context, id int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
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
		`DELETE FROM notification_deliveries WHERE rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_rule_channels WHERE alert_rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_events WHERE rule_id IN (SELECT id FROM alert_rules WHERE project_id = ?)`,
		`DELETE FROM alert_rules WHERE project_id = ?`,
		`DELETE FROM project_alert_config_channels WHERE project_alert_config_id IN (SELECT id FROM project_alert_configs WHERE project_id = ?)`,
		`DELETE FROM project_alert_configs WHERE project_id = ?`,
		`DELETE FROM project_api_tokens WHERE project_id = ?`,
		`DELETE FROM silences WHERE project_id = ?`,
		`DELETE FROM inhibition_rules WHERE project_id = ?`,
		`DELETE FROM telemetry_inbox WHERE project_id = ?`,
		`DELETE FROM issues WHERE project_id = ?`,
		`DELETE FROM replay_sessions WHERE project_id = ?`,
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
	if err := tx.Commit(); err != nil {
		return err
	}
	s.revision.Add(1)
	return nil
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

func tokenInfo(token *ProjectAPIToken) ProjectAPITokenInfo {
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

func userAPITokenInfo(token *UserAPIToken) UserAPITokenInfo {
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
		SetCreatedAt(input.CreatedAt).
		SetExpiresAt(input.ExpiresAt).
		Exec(ctx)
}

func (s *Store) AuthenticateAccountSession(ctx context.Context, token string, now time.Time) (int, error) {
	sess, err := s.client.Session.Query().Where(session.TokenEQ(storedCredential(token))).Only(ctx)
	if err != nil {
		return 0, err
	}
	if !sess.ExpiresAt.After(now) {
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
		Where(session.UserIDEQ(userID), session.ExpiresAtGT(now)).
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
		// Perform equivalent expensive work even when the username does not exist.
		_ = argon2Dummy(password)
		return UserInfo{}, errInvalidCredentials
	}
	if !VerifyPassword(u.PasswordHash, password) {
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
// fields for one administrator-managed user.
func (s *Store) UpdateManagedUser(ctx context.Context, input ManagedUserUpdate) (UserInfo, error) {
	var hash string
	var err error
	if input.Password != "" {
		hash, err = HashPassword(input.Password)
		if err != nil {
			return UserInfo{}, err
		}
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	defer func() { _ = tx.Rollback() }()

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
	result := userInfo(updated)
	if err := tx.Commit(); err != nil {
		return UserInfo{}, err
	}
	return result, nil
}

func (s *Store) DeleteUser(ctx context.Context, id int) error {
	return s.client.User.DeleteOneID(id).Exec(ctx)
}

// ChangePassword verifies the current password and stores the new hash.
// A wrong current password returns errInvalidCredentials.
func (s *Store) ChangePassword(ctx context.Context, userID int, currentPassword, newPassword string) error {
	u, err := s.client.User.Get(ctx, userID)
	if err != nil {
		return err
	}
	if !VerifyPassword(u.PasswordHash, currentPassword) {
		return errInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.client.User.UpdateOneID(userID).SetPasswordHash(hash).Exec(ctx)
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

// DeleteAuditBefore removes audit rows older than cutoff and returns the
// number of deleted rows.
func (s *Store) DeleteAuditBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return s.client.AuditEvent.Delete().Where(auditevent.CreatedAtLT(cutoff)).Exec(ctx)
}

func auditRecord(event *AuditEvent) AuditRecord {
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

func (s *Store) Bootstrap(ctx context.Context, logger *slog.Logger) (token string, err error) {
	release, err := s.acquireBootstrapCoordination(ctx)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, release()) }()
	if _, err := s.client.User.Query().Where(user.UsernameEQ("admin")).Only(ctx); IsNotFound(err) {
		password, genErr := randomPassword()
		if genErr != nil {
			return "", genErr
		}
		if _, createErr := s.CreateUser(ctx, "admin", password, string(user.RoleAdmin)); createErr != nil {
			return "", fmt.Errorf("create bootstrap admin: %w", createErr)
		}
		logger.Info("created bootstrap admin", "username", "admin", "password", password)
	} else if err != nil {
		return "", fmt.Errorf("find bootstrap admin: %w", err)
	}
	p, err := s.client.Project.Query().Where(project.SlugEQ("default")).Only(ctx)
	if IsNotFound(err) {
		p, err = s.CreateProject(ctx, "Default", "default")
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
	p, err := s.client.Project.Query().Where(project.SlugEQ(SelfMonitoringProjectSlug)).Only(ctx)
	if IsNotFound(err) {
		p, err = s.CreateProject(ctx, "lazy-observe", SelfMonitoringProjectSlug)
	}
	if err != nil {
		return ProjectInfo{}, "", fmt.Errorf("ensure self-monitoring project: %w", err)
	}
	info = ProjectInfo{ID: p.ID, Name: p.Name, Slug: p.Slug}
	existing, err := s.client.ProjectAPIToken.Query().
		Where(projectapitoken.TokenEQ(storedCredential(derived))).
		Only(ctx)
	switch {
	case err == nil:
		if existing.ProjectID != p.ID {
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
	if _, err := s.createProjectAPITokenValue(ctx, p.ID, "self-monitoring", derived); err != nil {
		return ProjectInfo{}, "", fmt.Errorf("create self-monitoring project API token: %w", err)
	}
	return info, derived, nil
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

func userInfo(u *User) UserInfo {
	info := UserInfo{ID: u.ID, Username: u.Username, Role: string(u.Role), CreatedAt: u.CreatedAt}
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
	ctx, cancel := context.WithTimeout(context.Background(), migrationUnlockTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, lockKey).Scan(&released)
	if err == nil && released {
		return nil
	}
	// A failed unlock must never return a session-scoped lock to database/sql's
	// pool. pgx stdlib exposes the underlying connection only for this discard.
	_ = conn.Raw(func(driverConn any) error {
		if pgxConn, ok := driverConn.(*pgxstdlib.Conn); ok {
			_ = pgxConn.Conn().Close(context.Background())
		}
		return driver.ErrBadConn
	})
	if err != nil {
		return fmt.Errorf("release %s lock: %w", operation, err)
	}
	return fmt.Errorf("release %s lock: lock was not held by the reserved connection", operation)
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
