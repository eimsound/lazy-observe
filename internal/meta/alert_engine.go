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

var (
	ErrInactiveAlertEngineMember = errors.New("alert-engine member is not active")
	ErrRuleLeaseHeld             = errors.New("alert rule lease is held by another member")
	ErrStaleRuleFence            = errors.New("alert rule fencing epoch is stale")
)

type AlertEngineMember struct {
	MemberID    string
	StartedAt   time.Time
	HeartbeatAt time.Time
	ExpiresAt   time.Time
}

type AlertRuleLease struct {
	RuleID    int
	OwnerID   string
	Epoch     int64
	ExpiresAt time.Time
	UpdatedAt time.Time
}

type AlertRuleState struct {
	RuleID          int
	Fingerprint     string
	Status          string
	PendingSince    *time.Time
	FiringSince     *time.Time
	LastEvaluatedAt time.Time
	Labels          map[string]string
	Annotations     map[string]string
	Value           *float64
	LastNotifiedAt  *time.Time
	LeaseEpoch      int64
}

func (s *Store) HeartbeatAlertEngine(
	ctx context.Context,
	memberID string,
	ttl time.Duration,
) (AlertEngineMember, error) {
	if err := validateAlertEngineMember(memberID, ttl); err != nil {
		return AlertEngineMember{}, err
	}
	expiry, expiryArg := s.alertLeaseExpiry(ttl)
	query := `INSERT INTO alert_engine_members(member_id, started_at, heartbeat_at, expires_at)
		VALUES (?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ` + expiry + `)
		ON CONFLICT(member_id) DO UPDATE SET heartbeat_at = CURRENT_TIMESTAMP, expires_at = excluded.expires_at
		RETURNING member_id, started_at, heartbeat_at, expires_at`
	var member AlertEngineMember
	if err := s.db.QueryRowContext(ctx, s.alertClockSQL(query), memberID, expiryArg).
		Scan(&member.MemberID, &member.StartedAt, &member.HeartbeatAt, &member.ExpiresAt); err != nil {
		return AlertEngineMember{}, fmt.Errorf("heartbeat alert engine member: %w", err)
	}
	return member, nil
}

func (s *Store) ActiveAlertEngineMembers(ctx context.Context) ([]AlertEngineMember, error) {
	rows, err := s.db.QueryContext(
		ctx,
		s.alertClockSQL(
			`SELECT member_id, started_at, heartbeat_at, expires_at FROM alert_engine_members WHERE expires_at > CURRENT_TIMESTAMP ORDER BY member_id`,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("list active alert engine members: %w", err)
	}
	defer rows.Close()
	var members []AlertEngineMember
	for rows.Next() {
		var member AlertEngineMember
		if err := rows.Scan(&member.MemberID, &member.StartedAt, &member.HeartbeatAt, &member.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan active alert engine member: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active alert engine members: %w", err)
	}
	return members, nil
}

func (s *Store) DeleteExpiredAlertEngineMembers(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(
		ctx,
		s.alertClockSQL(`DELETE FROM alert_engine_members WHERE expires_at <= CURRENT_TIMESTAMP`),
	)
	if err != nil {
		return 0, fmt.Errorf("delete expired alert engine members: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) DeleteAlertEngineMember(ctx context.Context, memberID string) (bool, error) {
	if memberID == "" || strings.TrimSpace(memberID) != memberID {
		return false, errors.New("alert-engine member ID must be non-empty and have no surrounding whitespace")
	}
	result, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM alert_engine_members WHERE member_id = ?`),
		memberID,
	)
	if err != nil {
		return false, fmt.Errorf("delete alert-engine member: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows != 0, err
}

func (s *Store) AcquireAlertRuleLease(
	ctx context.Context,
	ruleID int,
	memberID string,
	ttl time.Duration,
) (AlertRuleLease, error) {
	if ruleID <= 0 {
		return AlertRuleLease{}, errors.New("alert rule lease requires a positive rule ID")
	}
	if err := validateAlertEngineMember(memberID, ttl); err != nil {
		return AlertRuleLease{}, err
	}
	expiry, expiryArg := s.alertLeaseExpiry(ttl)
	query := `INSERT INTO alert_rule_leases(rule_id, owner_id, epoch, expires_at, updated_at)
		SELECT ?, ?, 1, ` + expiry + `, CURRENT_TIMESTAMP
		WHERE EXISTS (SELECT 1 FROM alert_engine_members WHERE member_id = ? AND expires_at > CURRENT_TIMESTAMP)
		AND EXISTS (SELECT 1 FROM alert_rules WHERE id = ? AND enabled = TRUE AND evaluation_engine <> 'paused')
		ON CONFLICT(rule_id) DO UPDATE SET
			owner_id = excluded.owner_id,
			epoch = CASE
				WHEN alert_rule_leases.owner_id = excluded.owner_id AND alert_rule_leases.expires_at > CURRENT_TIMESTAMP THEN alert_rule_leases.epoch
				ELSE alert_rule_leases.epoch + 1
			END,
			expires_at = excluded.expires_at,
			updated_at = CURRENT_TIMESTAMP
		WHERE alert_rule_leases.owner_id = excluded.owner_id OR alert_rule_leases.expires_at <= CURRENT_TIMESTAMP
		RETURNING rule_id, owner_id, epoch, expires_at, updated_at`
	var lease AlertRuleLease
	err := s.db.QueryRowContext(ctx, s.alertClockSQL(query), ruleID, memberID, expiryArg, memberID, ruleID).
		Scan(&lease.RuleID, &lease.OwnerID, &lease.Epoch, &lease.ExpiresAt, &lease.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		active, activeErr := s.alertEngineMemberActive(ctx, memberID)
		if activeErr != nil {
			return AlertRuleLease{}, activeErr
		}
		if !active {
			return AlertRuleLease{}, ErrInactiveAlertEngineMember
		}
		return AlertRuleLease{}, ErrRuleLeaseHeld
	}
	if err != nil {
		return AlertRuleLease{}, fmt.Errorf("acquire alert rule lease: %w", err)
	}
	return lease, nil
}

func (s *Store) PauseAlertRuleEvaluation(ctx context.Context, ruleID int) error {
	if ruleID <= 0 {
		return errors.New("alert rule pause requires a positive rule ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin alert rule pause: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(
		ctx,
		bind(
			s.dialect,
			`UPDATE alert_rules SET evaluation_engine = 'paused', updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		),
		ruleID,
	)
	if err != nil {
		return fmt.Errorf("pause alert rule evaluation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read alert rule pause result: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM alert_rule_leases WHERE rule_id = ?`),
		ruleID,
	); err != nil {
		return fmt.Errorf("revoke alert rule lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alert rule pause: %w", err)
	}
	return nil
}

func (s *Store) ActivateAlertRuleEvaluation(ctx context.Context, ruleID int, engine string) error {
	if ruleID <= 0 || engine != "native" {
		return errors.New("alert rule activation requires a positive rule ID and the native engine")
	}
	result, err := s.db.ExecContext(
		ctx,
		bind(
			s.dialect,
			`UPDATE alert_rules SET evaluation_engine = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND evaluation_engine = 'paused'`,
		),
		engine,
		ruleID,
	)
	if err != nil {
		return fmt.Errorf("activate alert rule evaluation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read alert rule activation result: %w", err)
	}
	if rows == 0 {
		return errors.New("alert rule is not paused")
	}
	return nil
}

func (s *Store) RenewAlertRuleLease(
	ctx context.Context,
	ruleID int,
	memberID string,
	epoch int64,
	ttl time.Duration,
) (AlertRuleLease, error) {
	if ruleID <= 0 || epoch <= 0 {
		return AlertRuleLease{}, errors.New("alert rule lease renewal requires positive rule ID and epoch")
	}
	if err := validateAlertEngineMember(memberID, ttl); err != nil {
		return AlertRuleLease{}, err
	}
	expiry, expiryArg := s.alertLeaseExpiry(ttl)
	query := `UPDATE alert_rule_leases SET expires_at = ` + expiry + `, updated_at = CURRENT_TIMESTAMP
		WHERE rule_id = ? AND owner_id = ? AND epoch = ? AND expires_at > CURRENT_TIMESTAMP
		AND EXISTS (SELECT 1 FROM alert_engine_members WHERE member_id = ? AND expires_at > CURRENT_TIMESTAMP)
		RETURNING rule_id, owner_id, epoch, expires_at, updated_at`
	var lease AlertRuleLease
	err := s.db.QueryRowContext(ctx, s.alertClockSQL(query), expiryArg, ruleID, memberID, epoch, memberID).
		Scan(&lease.RuleID, &lease.OwnerID, &lease.Epoch, &lease.ExpiresAt, &lease.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertRuleLease{}, ErrStaleRuleFence
	}
	if err != nil {
		return AlertRuleLease{}, fmt.Errorf("renew alert rule lease: %w", err)
	}
	return lease, nil
}

func (s *Store) ReleaseAlertRuleLease(ctx context.Context, ruleID int, memberID string, epoch int64) (bool, error) {
	result, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM alert_rule_leases WHERE rule_id = ? AND owner_id = ? AND epoch = ?`),
		ruleID,
		memberID,
		epoch,
	)
	if err != nil {
		return false, fmt.Errorf("release alert rule lease: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows != 0, err
}

func (s *Store) PutAlertRuleState(ctx context.Context, state AlertRuleState, memberID string, epoch int64) error {
	if err := validateAlertRuleState(state, memberID, epoch); err != nil {
		return err
	}
	if state.Labels == nil {
		state.Labels = map[string]string{}
	}
	if state.Annotations == nil {
		state.Annotations = map[string]string{}
	}
	labels, err := json.Marshal(state.Labels)
	if err != nil {
		return fmt.Errorf("encode alert state labels: %w", err)
	}
	annotations, err := json.Marshal(state.Annotations)
	if err != nil {
		return fmt.Errorf("encode alert state annotations: %w", err)
	}
	query := `INSERT INTO alert_rule_states(rule_id, fingerprint, status, pending_since, firing_since, last_evaluated_at, labels, annotations, value, last_notified_at, lease_epoch)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM alert_rule_leases WHERE rule_id = ? AND owner_id = ? AND epoch = ? AND expires_at > CURRENT_TIMESTAMP)
		ON CONFLICT(rule_id, fingerprint) DO UPDATE SET
			status = excluded.status, pending_since = excluded.pending_since, firing_since = excluded.firing_since,
			last_evaluated_at = excluded.last_evaluated_at, labels = excluded.labels, annotations = excluded.annotations,
			value = excluded.value, last_notified_at = excluded.last_notified_at, lease_epoch = excluded.lease_epoch
		WHERE EXISTS (SELECT 1 FROM alert_rule_leases WHERE rule_id = ? AND owner_id = ? AND epoch = ? AND expires_at > CURRENT_TIMESTAMP)`
	result, err := s.db.ExecContext(ctx, s.alertClockSQL(query),
		state.RuleID, state.Fingerprint, state.Status, state.PendingSince, state.FiringSince, state.LastEvaluatedAt,
		string(labels), string(annotations), state.Value, state.LastNotifiedAt, epoch,
		state.RuleID, memberID, epoch,
		state.RuleID, memberID, epoch,
	)
	if err != nil {
		return fmt.Errorf("persist fenced alert rule state: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read fenced alert state result: %w", err)
	}
	if rows == 0 {
		return ErrStaleRuleFence
	}
	return nil
}

func (s *Store) AlertRuleStates(ctx context.Context, ruleID int) ([]AlertRuleState, error) {
	rows, err := s.db.QueryContext(
		ctx,
		bind(
			s.dialect,
			`SELECT rule_id, fingerprint, status, pending_since, firing_since, last_evaluated_at, labels, annotations, value, last_notified_at, lease_epoch FROM alert_rule_states WHERE rule_id = ? ORDER BY fingerprint`,
		),
		ruleID,
	)
	if err != nil {
		return nil, fmt.Errorf("list alert rule states: %w", err)
	}
	defer rows.Close()
	var states []AlertRuleState
	for rows.Next() {
		var state AlertRuleState
		var pendingSince, firingSince, lastNotifiedAt sql.NullTime
		var value sql.NullFloat64
		var labels, annotations []byte
		if err := rows.Scan(
			&state.RuleID,
			&state.Fingerprint,
			&state.Status,
			&pendingSince,
			&firingSince,
			&state.LastEvaluatedAt,
			&labels,
			&annotations,
			&value,
			&lastNotifiedAt,
			&state.LeaseEpoch,
		); err != nil {
			return nil, fmt.Errorf("scan alert rule state: %w", err)
		}
		if pendingSince.Valid {
			state.PendingSince = &pendingSince.Time
		}
		if firingSince.Valid {
			state.FiringSince = &firingSince.Time
		}
		if value.Valid {
			state.Value = &value.Float64
		}
		if lastNotifiedAt.Valid {
			state.LastNotifiedAt = &lastNotifiedAt.Time
		}
		if err := json.Unmarshal(labels, &state.Labels); err != nil {
			return nil, fmt.Errorf("decode alert state labels: %w", err)
		}
		if err := json.Unmarshal(annotations, &state.Annotations); err != nil {
			return nil, fmt.Errorf("decode alert state annotations: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate alert rule states: %w", err)
	}
	return states, nil
}

func (s *Store) ProjectFiringAlertRuleStates(ctx context.Context, projectID int) ([]AlertRuleState, error) {
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, `SELECT state.rule_id, state.fingerprint, state.status,
		state.pending_since, state.firing_since, state.last_evaluated_at, state.labels, state.annotations,
		state.value, state.last_notified_at, state.lease_epoch FROM alert_rule_states AS state
		JOIN alert_rules AS rule ON rule.id = state.rule_id
		WHERE rule.project_id = ? AND state.status = 'firing' ORDER BY state.rule_id, state.fingerprint`), projectID)
	if err != nil {
		return nil, fmt.Errorf("list project firing alert states: %w", err)
	}
	defer rows.Close()
	return scanAlertRuleStates(rows)
}

func scanAlertRuleStates(rows *sql.Rows) ([]AlertRuleState, error) {
	var states []AlertRuleState
	for rows.Next() {
		var state AlertRuleState
		var pendingSince, firingSince, lastNotifiedAt sql.NullTime
		var value sql.NullFloat64
		var labels, annotations []byte
		if err := rows.Scan(&state.RuleID, &state.Fingerprint, &state.Status, &pendingSince, &firingSince,
			&state.LastEvaluatedAt, &labels, &annotations, &value, &lastNotifiedAt, &state.LeaseEpoch); err != nil {
			return nil, fmt.Errorf("scan alert rule state: %w", err)
		}
		if pendingSince.Valid {
			state.PendingSince = &pendingSince.Time
		}
		if firingSince.Valid {
			state.FiringSince = &firingSince.Time
		}
		if value.Valid {
			state.Value = &value.Float64
		}
		if lastNotifiedAt.Valid {
			state.LastNotifiedAt = &lastNotifiedAt.Time
		}
		if err := json.Unmarshal(labels, &state.Labels); err != nil {
			return nil, fmt.Errorf("decode alert state labels: %w", err)
		}
		if err := json.Unmarshal(annotations, &state.Annotations); err != nil {
			return nil, fmt.Errorf("decode alert state annotations: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate alert rule states: %w", err)
	}
	return states, nil
}

// sqliteAlertClock is SQLite's current time with millisecond precision. CURRENT_TIMESTAMP and
// datetime() truncate to whole seconds, which cut every lease and membership TTL short by up to
// a second: a 1s lease acquired at hh:mm:23.9 expired at hh:mm:24.
const sqliteAlertClock = `strftime('%Y-%m-%d %H:%M:%f', 'now')`

func (s *Store) alertLeaseExpiry(ttl time.Duration) (string, any) {
	if s.dialect == dialect.Postgres {
		return `CURRENT_TIMESTAMP + (? * INTERVAL '1 millisecond')`, ttl.Milliseconds()
	}
	return `strftime('%Y-%m-%d %H:%M:%f', 'now', ?)`, fmt.Sprintf("+%.6f seconds", ttl.Seconds())
}

// alertClockSQL binds a lease or membership query and, on SQLite, swaps CURRENT_TIMESTAMP for
// the same millisecond clock alertLeaseExpiry writes, so expiry comparisons keep sub-second TTLs.
// Rows written at whole-second precision still compare correctly: "hh:mm:24" sorts before
// "hh:mm:24.000", so they read as expired no later than they did before.
func (s *Store) alertClockSQL(query string) string {
	if s.dialect != dialect.Postgres {
		query = strings.ReplaceAll(query, "CURRENT_TIMESTAMP", sqliteAlertClock)
	}
	return bind(s.dialect, query)
}

func (s *Store) alertEngineMemberActive(ctx context.Context, memberID string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, s.alertClockSQL(`SELECT COUNT(*) FROM alert_engine_members WHERE member_id = ? AND expires_at > CURRENT_TIMESTAMP`), memberID).
		Scan(&count); err != nil {
		return false, fmt.Errorf("check active alert engine member: %w", err)
	}
	return count != 0, nil
}

func validateAlertEngineMember(memberID string, ttl time.Duration) error {
	if memberID == "" || strings.TrimSpace(memberID) != memberID {
		return errors.New("alert-engine member ID must be non-empty and have no surrounding whitespace")
	}
	if len(memberID) > 255 {
		return errors.New("alert-engine member ID must not exceed 255 bytes")
	}
	if ttl <= 0 {
		return errors.New("alert-engine TTL must be positive")
	}
	return nil
}

func validateAlertRuleState(state AlertRuleState, memberID string, epoch int64) error {
	if state.RuleID <= 0 || state.Fingerprint == "" || state.LastEvaluatedAt.IsZero() || epoch <= 0 {
		return errors.New("alert rule state requires rule ID, fingerprint, evaluation time, and fencing epoch")
	}
	if memberID == "" || strings.TrimSpace(memberID) != memberID {
		return errors.New("alert rule state requires a valid member ID")
	}
	if state.Status != "pending" && state.Status != "firing" && state.Status != "recovered" {
		return fmt.Errorf("unsupported alert rule state %q", state.Status)
	}
	return nil
}
