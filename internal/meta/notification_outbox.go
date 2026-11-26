package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
)

var ErrNotificationOutboxClaimLost = errors.New("notification outbox claim is no longer owned")

type NotificationOutboxEnqueue struct {
	IdempotencyKey string
	ProjectID      int
	RuleID         *int
	ChannelID      int
	Payload        json.RawMessage
	Priority       int
	// OrderingKey names the causal delivery stream of the row. Rows sharing an
	// OrderingKey and ChannelID are claimed strictly in id order: a row is not
	// claimable while an earlier row of its stream is pending or processing.
	// Empty means the row is unordered.
	OrderingKey string
}

type NotificationOutboxItem struct {
	ID             int64
	IdempotencyKey string
	ProjectID      int
	RuleID         *int
	ChannelID      int
	Payload        json.RawMessage
	Priority       int
	OrderingKey    string
	Status         string
	AvailableAt    time.Time
	ClaimOwner     string
	ClaimUntil     *time.Time
	Attempts       int
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeliveredAt    *time.Time
}

// notificationOutboxClaimable is the claim predicate over a notification_outbox
// row aliased item: the row is due (pending past available_at, or processing
// with an expired claim) and, when it carries an ordering_key, no earlier row
// of the same (ordering_key, channel_id) stream is still pending or
// processing. Delivered and dead predecessors release their successors, so a
// firing notification that exhausted its retries cannot block its resolved
// notification forever (issue #386). Because a blocked row is not claimable
// while its predecessor is, one claim batch never holds two rows of a stream
// and OutboxWorker.drain may deliver a batch concurrently.
const notificationOutboxClaimable = `((item.status = 'pending' AND item.available_at <= CURRENT_TIMESTAMP)
		OR (item.status = 'processing' AND item.claim_until <= CURRENT_TIMESTAMP))
	AND (item.ordering_key IS NULL OR NOT EXISTS (
		SELECT 1 FROM notification_outbox AS prior
		WHERE prior.ordering_key = item.ordering_key AND prior.channel_id = item.channel_id
			AND prior.status IN ('pending', 'processing') AND prior.id < item.id))`

func nullableOutboxOrderingKey(key string) any {
	if key == "" {
		return nil
	}
	return key
}

func (s *Store) NotificationOutboxItem(ctx context.Context, id int64) (NotificationOutboxItem, error) {
	var item NotificationOutboxItem
	query := `SELECT id, idempotency_key, project_id, rule_id, channel_id, payload, priority, ordering_key, status,
		available_at, claim_owner, claim_until, attempts, last_error, created_at, updated_at, delivered_at
		FROM notification_outbox WHERE id = ?`
	if err := scanNotificationOutbox(s.db.QueryRowContext(ctx, bind(s.dialect, query), id), &item); err != nil {
		return NotificationOutboxItem{}, fmt.Errorf("read notification outbox item %d: %w", id, err)
	}
	return item, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const insertNotificationOutboxSQL = `INSERT INTO notification_outbox(idempotency_key, project_id, rule_id, channel_id, payload, priority, ordering_key, status, available_at, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT(idempotency_key) DO NOTHING RETURNING id`

func validateNotificationOutboxEnqueue(input NotificationOutboxEnqueue) error {
	if input.IdempotencyKey == "" || input.ProjectID <= 0 || input.ChannelID <= 0 || !json.Valid(input.Payload) {
		return errors.New(
			"notification outbox requires idempotency key, project, channel, and valid JSON payload",
		)
	}
	if input.RuleID != nil && *input.RuleID <= 0 {
		return errors.New("notification outbox rule ID must be positive when present")
	}
	return nil
}

func (s *Store) insertNotificationOutbox(
	ctx context.Context,
	qr rowQuerier,
	input NotificationOutboxEnqueue,
) (id int64, inserted bool, err error) {
	err = qr.QueryRowContext(
		ctx,
		bind(s.dialect, insertNotificationOutboxSQL),
		input.IdempotencyKey,
		input.ProjectID,
		input.RuleID,
		input.ChannelID,
		string(input.Payload),
		input.Priority,
		nullableOutboxOrderingKey(input.OrderingKey),
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("enqueue notification outbox: %w", err)
}

func (s *Store) EnqueueNotification(
	ctx context.Context,
	input NotificationOutboxEnqueue,
) (id int64, inserted bool, err error) {
	if err := validateNotificationOutboxEnqueue(input); err != nil {
		return 0, false, err
	}
	id, inserted, err = s.insertNotificationOutbox(ctx, s.db, input)
	if err != nil || inserted {
		return id, inserted, err
	}
	if err := s.db.QueryRowContext(ctx, bind(s.dialect, `SELECT id FROM notification_outbox WHERE idempotency_key = ?`), input.IdempotencyKey).
		Scan(&id); err != nil {
		return 0, false, fmt.Errorf("read existing notification outbox item: %w", err)
	}
	return id, false, nil
}

func (s *Store) ClaimNotifications(
	ctx context.Context,
	owner string,
	limit int,
	ttl time.Duration,
) ([]NotificationOutboxItem, error) {
	if owner == "" || limit <= 0 || ttl <= 0 {
		return nil, errors.New("notification outbox claim requires owner, positive limit, and positive TTL")
	}
	if limit > 100 {
		limit = 100
	}
	if s.dialect == dialect.Postgres {
		return s.claimPostgresNotifications(ctx, owner, limit, ttl)
	}
	return s.claimSQLiteNotifications(ctx, owner, limit, ttl)
}

func (s *Store) claimPostgresNotifications(
	ctx context.Context,
	owner string,
	limit int,
	ttl time.Duration,
) ([]NotificationOutboxItem, error) {
	query := `WITH picked AS (
		SELECT id FROM notification_outbox AS item
		WHERE ` + notificationOutboxClaimable + `
		ORDER BY priority DESC, available_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT ?
	)
	UPDATE notification_outbox AS item SET
		status = 'processing', claim_owner = ?, claim_until = CURRENT_TIMESTAMP + (? * INTERVAL '1 millisecond'),
		attempts = attempts + 1, updated_at = CURRENT_TIMESTAMP
	FROM picked WHERE item.id = picked.id
	RETURNING item.id, item.idempotency_key, item.project_id, item.rule_id, item.channel_id, item.payload,
		item.priority, item.ordering_key, item.status, item.available_at, item.claim_owner, item.claim_until, item.attempts,
		item.last_error, item.created_at, item.updated_at, item.delivered_at`
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, query), limit, owner, ttl.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claim PostgreSQL notification outbox: %w", err)
	}
	defer rows.Close()
	return scanNotificationOutboxRows(rows)
}

func selectClaimableSQLiteNotificationIDsTx(ctx context.Context, tx *sql.Tx, limit int) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM notification_outbox AS item
		WHERE `+notificationOutboxClaimable+`
		ORDER BY priority DESC, available_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("select SQLite notification outbox: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan SQLite notification outbox ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SQLite notification outbox IDs: %w", err)
	}
	return ids, nil
}

func (s *Store) claimSQLiteNotifications(
	ctx context.Context,
	owner string,
	limit int,
	ttl time.Duration,
) ([]NotificationOutboxItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin SQLite notification outbox claim: %w", err)
	}
	defer tx.Rollback()
	ids, err := selectClaimableSQLiteNotificationIDsTx(ctx, tx, limit)
	if err != nil {
		return nil, err
	}
	var items []NotificationOutboxItem
	for _, id := range ids {
		var item NotificationOutboxItem
		query := `UPDATE notification_outbox AS item SET status = 'processing', claim_owner = ?,
			claim_until = datetime('now', ?), attempts = attempts + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND ` + notificationOutboxClaimable + `
			RETURNING id, idempotency_key, project_id, rule_id, channel_id, payload, priority, ordering_key, status,
				available_at, claim_owner, claim_until, attempts, last_error, created_at, updated_at, delivered_at`
		expiry := fmt.Sprintf("+%.6f seconds", ttl.Seconds())
		if err := scanNotificationOutbox(
			tx.QueryRowContext(ctx, query, owner, expiry, id),
			&item,
		); errors.Is(
			err,
			sql.ErrNoRows,
		) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("claim SQLite notification outbox item %d: %w", id, err)
		}
		items = append(items, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite notification outbox claim: %w", err)
	}
	return items, nil
}

func (s *Store) CompleteNotification(ctx context.Context, id int64, owner string) error {
	result, err := s.db.ExecContext(
		ctx,
		bind(
			s.dialect,
			`UPDATE notification_outbox SET status = 'delivered', claim_owner = NULL, claim_until = NULL, last_error = NULL, delivered_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = 'processing' AND claim_owner = ?`,
		),
		id,
		owner,
	)
	if err != nil {
		return fmt.Errorf("complete notification outbox item: %w", err)
	}
	return requireOutboxClaim(result)
}

func (s *Store) RetryNotification(
	ctx context.Context,
	id int64,
	owner, message string,
	delay time.Duration,
	maxAttempts int,
) (string, error) {
	if delay <= 0 || maxAttempts <= 0 {
		return "", errors.New("notification retry requires positive delay and max attempts")
	}
	expiry, expiryArg := s.notificationRetryTime(delay)
	query := `UPDATE notification_outbox SET
		status = CASE WHEN attempts >= ? THEN 'dead' ELSE 'pending' END,
		available_at = ` + expiry + `, claim_owner = NULL, claim_until = NULL,
		last_error = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'processing' AND claim_owner = ?
		RETURNING status`
	var status string
	err := s.db.QueryRowContext(ctx, bind(s.dialect, query), maxAttempts, expiryArg, message, id, owner).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotificationOutboxClaimLost
	}
	if err != nil {
		return "", fmt.Errorf("retry notification outbox item: %w", err)
	}
	return status, nil
}

// DeferNotification releases a claim without consuming an attempt. Workers use
// it when a rate limiter delays delivery before any external send is attempted.
func (s *Store) DeferNotification(ctx context.Context, id int64, owner string, delay time.Duration) error {
	if delay <= 0 {
		return errors.New("notification deferral requires a positive delay")
	}
	expiry, expiryArg := s.notificationRetryTime(delay)
	query := `UPDATE notification_outbox SET status = 'pending', available_at = ` + expiry + `,
		claim_owner = NULL, claim_until = NULL, attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
		updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = 'processing' AND claim_owner = ?`
	result, err := s.db.ExecContext(ctx, bind(s.dialect, query), expiryArg, id, owner)
	if err != nil {
		return fmt.Errorf("defer notification outbox item: %w", err)
	}
	return requireOutboxClaim(result)
}

func (s *Store) notificationRetryTime(delay time.Duration) (string, any) {
	if s.dialect == dialect.Postgres {
		return `CURRENT_TIMESTAMP + (? * INTERVAL '1 millisecond')`, delay.Milliseconds()
	}
	return `datetime('now', ?)`, fmt.Sprintf("+%.6f seconds", delay.Seconds())
}

func requireOutboxClaim(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read notification outbox result: %w", err)
	}
	if rows == 0 {
		return ErrNotificationOutboxClaimLost
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanNotificationOutbox(scanner rowScanner, item *NotificationOutboxItem) error {
	var ruleID sql.NullInt64
	var claimOwner, lastError, orderingKey sql.NullString
	var claimUntil, deliveredAt sql.NullTime
	var payload []byte
	if err := scanner.Scan(
		&item.ID, &item.IdempotencyKey, &item.ProjectID, &ruleID, &item.ChannelID, &payload,
		&item.Priority, &orderingKey, &item.Status, &item.AvailableAt, &claimOwner, &claimUntil, &item.Attempts,
		&lastError, &item.CreatedAt, &item.UpdatedAt, &deliveredAt,
	); err != nil {
		return err
	}
	if ruleID.Valid {
		value := int(ruleID.Int64)
		item.RuleID = &value
	}
	item.Payload = append(item.Payload[:0], payload...)
	item.ClaimOwner = claimOwner.String
	item.OrderingKey = orderingKey.String
	item.LastError = lastError.String
	if claimUntil.Valid {
		item.ClaimUntil = &claimUntil.Time
	}
	if deliveredAt.Valid {
		item.DeliveredAt = &deliveredAt.Time
	}
	return nil
}

func scanNotificationOutboxRows(rows *sql.Rows) ([]NotificationOutboxItem, error) {
	var items []NotificationOutboxItem
	for rows.Next() {
		var item NotificationOutboxItem
		if err := scanNotificationOutbox(rows, &item); err != nil {
			return nil, fmt.Errorf("scan notification outbox item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification outbox items: %w", err)
	}
	return items, nil
}

type NotificationOutboxListOptions struct {
	ProjectID int
	ChannelID int
	Status    string
	BeforeID  int64
	Limit     int
}

type NotificationOutboxView struct {
	ID          int64      `json:"id"`
	ProjectID   int        `json:"project_id"`
	RuleID      *int       `json:"rule_id"`
	ChannelID   int        `json:"channel_id"`
	ChannelName string     `json:"channel_name"`
	ChannelType string     `json:"channel_type"`
	Priority    int        `json:"priority"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error"`
	AvailableAt time.Time  `json:"available_at"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

// ListNotificationOutbox returns outbox rows newest-first, joined with their
// channel, scoped to one project.
func (s *Store) ListNotificationOutbox(
	ctx context.Context,
	options NotificationOutboxListOptions,
) ([]NotificationOutboxView, error) {
	limit := options.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query := `SELECT o.id, o.project_id, o.rule_id, o.channel_id, COALESCE(c.name, ''), COALESCE(c.type, ''),
		o.priority, o.status, o.attempts, COALESCE(o.last_error, ''), o.available_at, o.created_at, o.delivered_at
		FROM notification_outbox o
		LEFT JOIN notification_channels c ON c.id = o.channel_id
		WHERE o.project_id = ?`
	args := []any{options.ProjectID}
	if options.ChannelID > 0 {
		query += " AND o.channel_id = ?"
		args = append(args, options.ChannelID)
	}
	if options.Status != "" {
		query += " AND o.status = ?"
		args = append(args, options.Status)
	}
	if options.BeforeID > 0 {
		query += " AND o.id < ?"
		args = append(args, options.BeforeID)
	}
	query += " ORDER BY o.id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("list notification outbox: %w", err)
	}
	defer rows.Close()
	result := make([]NotificationOutboxView, 0, limit)
	for rows.Next() {
		var item NotificationOutboxView
		if err := rows.Scan(
			&item.ID,
			&item.ProjectID,
			&item.RuleID,
			&item.ChannelID,
			&item.ChannelName,
			&item.ChannelType,
			&item.Priority,
			&item.Status,
			&item.Attempts,
			&item.LastError,
			&item.AvailableAt,
			&item.CreatedAt,
			&item.DeliveredAt,
		); err != nil {
			return nil, fmt.Errorf("scan notification outbox: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification outbox: %w", err)
	}
	return result, nil
}

// RequeueNotification moves a dead notification back to pending so the
// dispatch workers pick it up again. Attempts are reset to 0 and the database
// clock is used so eligibility matches ClaimNotifications across SQLite and PG.
// Only dead rows of the project are requeued; the returned bool reports
// whether one row changed.
//
// Note: manually requeuing a dead row that carries an OrderingKey restores it
// to pending, so it can deliver after successors of the same stream that were
// already released when it became dead.
func (s *Store) RequeueNotification(ctx context.Context, id int64, projectID int) (bool, error) {
	result, err := s.db.ExecContext(
		ctx,
		bind(
			s.dialect,
			`UPDATE notification_outbox
			 SET status = 'pending', attempts = 0, available_at = CURRENT_TIMESTAMP,
			     claim_owner = NULL, claim_until = NULL, updated_at = CURRENT_TIMESTAMP
			 WHERE id = ? AND project_id = ? AND status = 'dead'`,
		),
		id,
		projectID,
	)
	if err != nil {
		return false, fmt.Errorf("requeue notification %d: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("requeue notification %d rows: %w", id, err)
	}
	return count > 0, nil
}

// NotificationOutboxDepth summarizes live queue backlog across outbox states.
type NotificationOutboxDepth struct {
	Pending    int64 `json:"pending"`
	Processing int64 `json:"processing"`
	Dead       int64 `json:"dead"`
	Due        int64 `json:"due"`
}

// NotificationOutboxDepth returns the current pending, processing, dead, and
// immediately claimable (due) item counts in notification_outbox.
func (s *Store) NotificationOutboxDepth(ctx context.Context) (NotificationOutboxDepth, error) {
	var depth NotificationOutboxDepth
	query := `SELECT
		COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'processing' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'dead' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + notificationOutboxClaimable + ` THEN 1 ELSE 0 END), 0)
		FROM notification_outbox AS item`
	if err := s.db.QueryRowContext(ctx, bind(s.dialect, query)).
		Scan(&depth.Pending, &depth.Processing, &depth.Dead, &depth.Due); err != nil {
		return NotificationOutboxDepth{}, fmt.Errorf("query notification outbox depth: %w", err)
	}
	return depth, nil
}

const notificationOutboxKeyLookupChunkSize = 500

// NotificationOutboxExistingKeys returns the subset of keys that already exist
// in notification_outbox regardless of row status. Callers use it to verify
// that a prior transition (such as a cycle's firing alert) was enqueued before
// enqueueing a follow-up resolved notification.
func (s *Store) NotificationOutboxExistingKeys(
	ctx context.Context,
	keys []string,
) (map[string]struct{}, error) {
	existing := make(map[string]struct{})
	if len(keys) == 0 {
		return existing, nil
	}
	unique := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	if len(unique) == 0 {
		return existing, nil
	}
	for start := 0; start < len(unique); start += notificationOutboxKeyLookupChunkSize {
		end := start + notificationOutboxKeyLookupChunkSize
		if end > len(unique) {
			end = len(unique)
		}
		if err := s.readExistingNotificationOutboxKeysChunk(ctx, unique[start:end], existing); err != nil {
			return nil, err
		}
	}
	return existing, nil
}

func (s *Store) readExistingNotificationOutboxKeysChunk(
	ctx context.Context,
	chunk []string,
	existing map[string]struct{},
) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
	args := make([]any, len(chunk))
	for i, key := range chunk {
		args[i] = key
	}
	query := bind(
		s.dialect,
		`SELECT idempotency_key FROM notification_outbox WHERE idempotency_key IN (`+placeholders+`)`,
	)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("check existing notification outbox keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return fmt.Errorf("scan existing notification outbox key: %w", err)
		}
		existing[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate existing notification outbox keys: %w", err)
	}
	return nil
}
