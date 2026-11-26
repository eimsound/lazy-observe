package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"entgo.io/ent/dialect"
)

type NativeAlertEvaluation struct {
	RuleID                int
	MemberID              string
	LeaseEpoch            int64
	States                []AlertRuleState
	PruneFingerprints     []string
	InhibitedFingerprints map[string]bool
	Notifications         []NotificationOutboxEnqueue
}

// CommitNativeAlertEvaluation advances candidate state and creates durable
// notification work under the same fenced transaction. The rule row and lease
// are locked in consistent order (rule then lease) before writes so a
// concurrent rule update or engine cutover is ordered cleanly without deadlock.
func (s *Store) CommitNativeAlertEvaluation(ctx context.Context, evaluation NativeAlertEvaluation) error {
	if evaluation.RuleID <= 0 || evaluation.MemberID == "" || evaluation.LeaseEpoch <= 0 {
		return errors.New("native alert evaluation requires rule, member, and fencing epoch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin native alert evaluation: %w", err)
	}
	defer tx.Rollback()
	lockQuery := `SELECT lease.epoch FROM alert_rules AS rule
		JOIN alert_rule_leases AS lease ON lease.rule_id = rule.id
		WHERE lease.rule_id = ? AND lease.owner_id = ? AND lease.epoch = ?
		AND lease.expires_at > CURRENT_TIMESTAMP AND rule.enabled = TRUE AND rule.evaluation_engine = 'native'`
	if s.dialect == dialect.Postgres {
		lockQuery += ` FOR UPDATE OF rule, lease`
	}
	var epoch int64
	if err := tx.QueryRowContext(ctx, s.alertClockSQL(lockQuery), evaluation.RuleID, evaluation.MemberID, evaluation.LeaseEpoch).
		Scan(&epoch); errors.Is(
		err,
		sql.ErrNoRows,
	) {
		return ErrStaleRuleFence
	} else if err != nil {
		return fmt.Errorf("lock native alert authority: %w", err)
	}
	for _, state := range evaluation.States {
		if state.RuleID != evaluation.RuleID {
			return errors.New("native alert state belongs to another rule")
		}
		if err := validateAlertRuleState(state, evaluation.MemberID, evaluation.LeaseEpoch); err != nil {
			return err
		}
		labels, err := json.Marshal(nonNilStringMap(state.Labels))
		if err != nil {
			return fmt.Errorf("encode native alert labels: %w", err)
		}
		annotations, err := json.Marshal(nonNilStringMap(state.Annotations))
		if err != nil {
			return fmt.Errorf("encode native alert annotations: %w", err)
		}
		if state.Status == "recovered" {
			if state.FiringSince != nil {
				resolveQuery := `INSERT INTO alert_events(fingerprint, status, labels, annotations, starts_at, ends_at, inhibited, updated_at, rule_id)
					VALUES (?, 'resolved', ?, ?, ?, ?, FALSE, CURRENT_TIMESTAMP, ?)
					ON CONFLICT(fingerprint, rule_id) DO UPDATE SET status = 'resolved', labels = excluded.labels,
					annotations = excluded.annotations, starts_at = excluded.starts_at, ends_at = excluded.ends_at,
					inhibited = FALSE, updated_at = CURRENT_TIMESTAMP
					WHERE alert_events.status <> 'resolved'`
				if _, err := tx.ExecContext(ctx, bind(s.dialect, resolveQuery), state.Fingerprint, string(labels),
					string(annotations), state.FiringSince, state.LastEvaluatedAt, state.RuleID); err != nil {
					return fmt.Errorf("resolve native alert event: %w", err)
				}
			}
			if _, err := tx.ExecContext(
				ctx,
				bind(s.dialect, `DELETE FROM alert_rule_states WHERE rule_id = ? AND fingerprint = ?`),
				state.RuleID,
				state.Fingerprint,
			); err != nil {
				return fmt.Errorf("prune recovered alert state: %w", err)
			}
			continue
		}
		query := `INSERT INTO alert_rule_states(rule_id, fingerprint, status, pending_since, firing_since, last_evaluated_at,
			labels, annotations, value, last_notified_at, lease_epoch) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(rule_id, fingerprint) DO UPDATE SET status = excluded.status, pending_since = excluded.pending_since,
			firing_since = excluded.firing_since, last_evaluated_at = excluded.last_evaluated_at, labels = excluded.labels,
			annotations = excluded.annotations, value = excluded.value, last_notified_at = excluded.last_notified_at,
			lease_epoch = excluded.lease_epoch`
		if _, err := tx.ExecContext(
			ctx,
			bind(s.dialect, query),
			state.RuleID,
			state.Fingerprint,
			state.Status,
			state.PendingSince,
			state.FiringSince,
			state.LastEvaluatedAt,
			string(labels),
			string(annotations),
			state.Value,
			state.LastNotifiedAt,
			evaluation.LeaseEpoch,
		); err != nil {
			return fmt.Errorf("persist native alert state: %w", err)
		}
		if state.Status == "pending" || state.FiringSince == nil {
			continue
		}
		inhibited, hasInhibited := evaluation.InhibitedFingerprints[state.Fingerprint]
		if hasInhibited {
			eventQuery := `INSERT INTO alert_events(fingerprint, status, labels, annotations, starts_at, ends_at, inhibited, updated_at, rule_id)
				VALUES (?, 'firing', ?, ?, ?, NULL, ?, CURRENT_TIMESTAMP, ?)
				ON CONFLICT(fingerprint, rule_id) DO UPDATE SET status = 'firing', labels = excluded.labels,
				annotations = excluded.annotations, starts_at = excluded.starts_at, ends_at = NULL,
				inhibited = excluded.inhibited, updated_at = CURRENT_TIMESTAMP`
			if _, err := tx.ExecContext(ctx, bind(s.dialect, eventQuery), state.Fingerprint, string(labels),
				string(annotations), state.FiringSince, inhibited, state.RuleID); err != nil {
				return fmt.Errorf("persist native alert event: %w", err)
			}
		} else {
			eventQuery := `INSERT INTO alert_events(fingerprint, status, labels, annotations, starts_at, ends_at, inhibited, updated_at, rule_id)
				VALUES (?, 'firing', ?, ?, ?, NULL, FALSE, CURRENT_TIMESTAMP, ?)
				ON CONFLICT(fingerprint, rule_id) DO UPDATE SET status = 'firing', labels = excluded.labels,
				annotations = excluded.annotations, starts_at = excluded.starts_at, ends_at = NULL,
				updated_at = CURRENT_TIMESTAMP`
			if _, err := tx.ExecContext(ctx, bind(s.dialect, eventQuery), state.Fingerprint, string(labels),
				string(annotations), state.FiringSince, state.RuleID); err != nil {
				return fmt.Errorf("persist native alert event: %w", err)
			}
		}
	}
	for _, fingerprint := range evaluation.PruneFingerprints {
		if fingerprint == "" {
			continue
		}
		if _, err := tx.ExecContext(
			ctx,
			bind(s.dialect, `DELETE FROM alert_rule_states WHERE rule_id = ? AND fingerprint = ?`),
			evaluation.RuleID,
			fingerprint,
		); err != nil {
			return fmt.Errorf("prune stale alert state: %w", err)
		}
	}
	for _, notification := range evaluation.Notifications {
		if notification.RuleID == nil || *notification.RuleID != evaluation.RuleID ||
			notification.IdempotencyKey == "" ||
			notification.ProjectID <= 0 ||
			notification.ChannelID <= 0 ||
			!json.Valid(notification.Payload) {
			return errors.New("native alert notification is invalid or belongs to another rule")
		}
		query := `INSERT INTO notification_outbox(idempotency_key, project_id, rule_id, channel_id, payload, priority,
			ordering_key, status, available_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(idempotency_key) DO NOTHING`
		if _, err := tx.ExecContext(
			ctx,
			bind(s.dialect, query),
			notification.IdempotencyKey,
			notification.ProjectID,
			notification.RuleID,
			notification.ChannelID,
			string(notification.Payload),
			notification.Priority,
			nullableOutboxOrderingKey(notification.OrderingKey),
		); err != nil {
			return fmt.Errorf("enqueue native alert notification: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit native alert evaluation: %w", err)
	}
	return nil
}

func nonNilStringMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	return value
}
