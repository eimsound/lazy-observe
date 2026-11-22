package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/eimsound/lazy-observe/internal/errortrack"
)

type inboxEventKey struct {
	projectID int
	eventID   string
}

type inboxQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

const inboxLookupChunkSize = 500

func (s *Store) existingInboxDigests(
	ctx context.Context,
	querier inboxQuerier,
	entries []errortrack.InboxEntry,
) (map[inboxEventKey]string, error) {
	byProject := make(map[int][]string)
	for _, entry := range entries {
		byProject[entry.ProjectID] = append(byProject[entry.ProjectID], entry.EventID)
	}
	existing := make(map[inboxEventKey]string)
	for projectID, eventIDs := range byProject {
		for start := 0; start < len(eventIDs); start += inboxLookupChunkSize {
			end := min(start+inboxLookupChunkSize, len(eventIDs))
			if err := s.readExistingInboxDigestsChunk(
				ctx,
				querier,
				projectID,
				eventIDs[start:end],
				existing,
			); err != nil {
				return nil, err
			}
		}
	}
	return existing, nil
}

func (s *Store) readExistingInboxDigestsChunk(
	ctx context.Context,
	querier inboxQuerier,
	projectID int,
	chunk []string,
	existing map[inboxEventKey]string,
) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
	args := make([]any, 0, len(chunk)+1)
	args = append(args, projectID)
	for _, eventID := range chunk {
		args = append(args, eventID)
	}
	query := bind(
		s.dialect,
		`SELECT event_id, canonical_digest FROM telemetry_inbox WHERE project_id = ? AND event_id IN (`+
			placeholders+`)`,
	)
	rows, err := querier.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read accepted error events for project %d: %w", projectID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID, digest string
		if err := rows.Scan(&eventID, &digest); err != nil {
			return fmt.Errorf("scan accepted error event for project %d: %w", projectID, err)
		}
		existing[inboxEventKey{projectID: projectID, eventID: eventID}] = digest
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate accepted error events for project %d: %w", projectID, err)
	}
	return nil
}

func filterPendingInboxEntries(
	entries []errortrack.InboxEntry,
	existing map[inboxEventKey]string,
) ([]errortrack.InboxEntry, error) {
	pending := make([]errortrack.InboxEntry, 0, len(entries))
	for _, entry := range entries {
		if digest, ok := existing[inboxEventKey{projectID: entry.ProjectID, eventID: entry.EventID}]; ok {
			if digest != entry.CanonicalDigest {
				return nil, fmt.Errorf(
					"%w: project %d event %q",
					errortrack.ErrConflict,
					entry.ProjectID,
					entry.EventID,
				)
			}
			continue
		}
		pending = append(pending, entry)
	}
	return pending, nil
}

func exceedsInboxCapacity(
	state errorInboxState,
	entries []errortrack.InboxEntry,
	capacity errortrack.InboxCapacity,
) bool {
	if int64(len(entries)) > capacity.MaxItems-state.activeItems {
		return true
	}
	var newBytes int64
	for _, entry := range entries {
		if entry.PayloadBytes > capacity.MaxBytes || newBytes > capacity.MaxBytes-entry.PayloadBytes {
			return true
		}
		newBytes += entry.PayloadBytes
	}
	return state.activeBytes > capacity.MaxBytes-newBytes
}

func (s *Store) AcceptErrorEvents(
	ctx context.Context,
	entries []errortrack.InboxEntry,
	capacity errortrack.InboxCapacity,
) error {
	if len(entries) == 0 || capacity.MaxItems <= 0 || capacity.MaxBytes <= 0 {
		return fmt.Errorf("%w: error batch and capacity are required", errortrack.ErrInvalid)
	}
	seen := make(map[inboxEventKey]string, len(entries))
	unique := make([]errortrack.InboxEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.ProjectID <= 0 || entry.EventID == "" || entry.CanonicalDigest == "" || len(entry.Payload) == 0 ||
			entry.PayloadBytes != int64(len(entry.Payload)) || entry.OccurredAt.IsZero() || entry.AcceptedAt.IsZero() {
			return fmt.Errorf("%w: incomplete error Inbox entry", errortrack.ErrInvalid)
		}
		key := inboxEventKey{projectID: entry.ProjectID, eventID: entry.EventID}
		if digest, ok := seen[key]; ok {
			if digest != entry.CanonicalDigest {
				return fmt.Errorf("%w: project %d event %q", errortrack.ErrConflict, entry.ProjectID, entry.EventID)
			}
			continue
		}
		seen[key] = entry.CanonicalDigest
		unique = append(unique, entry)
	}
	// Perform idempotency lookup outside the telemetry_inbox_state exclusive lock
	// so duplicate retries and conflicts never contend on the global state row
	// and normal inserts do not hold the state lock across N lookup queries.
	existing, err := s.existingInboxDigests(ctx, s.db, unique)
	if err != nil {
		return err
	}
	newEntries, err := filterPendingInboxEntries(unique, existing)
	if err != nil {
		return err
	}
	if len(newEntries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin error Inbox acceptance: %w", err)
	}
	defer tx.Rollback()
	state, err := s.lockErrorInboxStateTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("lock error Inbox capacity: %w", err)
	}
	if exceedsInboxCapacity(state, newEntries, capacity) {
		// Re-check under the state lock on the slow capacity-hit path so a
		// concurrent duplicate or conflict is not misclassified as capacity full.
		txExisting, lookupErr := s.existingInboxDigests(ctx, tx, newEntries)
		if lookupErr != nil {
			return lookupErr
		}
		newEntries, lookupErr = filterPendingInboxEntries(newEntries, txExisting)
		if lookupErr != nil {
			return lookupErr
		}
		if len(newEntries) == 0 {
			return tx.Commit()
		}
		if exceedsInboxCapacity(state, newEntries, capacity) {
			if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
				rejections_total = rejections_total + 1, updated_at = ? WHERE id = 1`), unique[0].AcceptedAt.UTC()); err != nil {
				return fmt.Errorf("record error Inbox capacity rejection: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit error Inbox capacity rejection: %w", err)
			}
			return errortrack.ErrCapacity
		}
	}
	snapshots := make(map[string]string)
	var insertedCount int
	var insertedBytes int64
	insertQuery := bind(s.dialect, `INSERT INTO telemetry_inbox(
		event_id, canonical_digest, payload, payload_bytes, occurred_at, accepted_at, source_map_snapshot,
		status, available_at, claim_owner, claim_epoch, attempts, last_error, outcome, created_at, updated_at, project_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, '', 0, 0, '', '', ?, ?, ?)
	ON CONFLICT(project_id, event_id) DO NOTHING
	RETURNING id`)
	digestQuery := bind(
		s.dialect,
		`SELECT canonical_digest FROM telemetry_inbox WHERE project_id = ? AND event_id = ?`,
	)
	for _, entry := range newEntries {
		snapshotKey := fmt.Sprintf("%d\x00%s", entry.ProjectID, entry.Release)
		snapshot, ok := snapshots[snapshotKey]
		if !ok {
			snapshotJSON, snapshotErr := s.sourceMapSnapshotTx(ctx, tx, entry.ProjectID, entry.Release)
			if snapshotErr != nil {
				return fmt.Errorf("capture source map snapshot for event %q: %w", entry.EventID, snapshotErr)
			}
			snapshot = string(snapshotJSON)
			snapshots[snapshotKey] = snapshot
		}
		var insertedID int64
		err := tx.QueryRowContext(
			ctx,
			insertQuery,
			entry.EventID,
			entry.CanonicalDigest,
			entry.Payload,
			entry.PayloadBytes,
			entry.OccurredAt.UTC(),
			entry.AcceptedAt.UTC(),
			snapshot,
			entry.AcceptedAt.UTC(),
			entry.AcceptedAt.UTC(),
			entry.AcceptedAt.UTC(),
			entry.ProjectID,
		).Scan(&insertedID)
		if errors.Is(err, sql.ErrNoRows) {
			var digest string
			if scanErr := tx.QueryRowContext(ctx, digestQuery, entry.ProjectID, entry.EventID).
				Scan(&digest); scanErr != nil {
				return fmt.Errorf("read concurrently accepted error event %q: %w", entry.EventID, scanErr)
			}
			if digest != entry.CanonicalDigest {
				return fmt.Errorf("%w: project %d event %q", errortrack.ErrConflict, entry.ProjectID, entry.EventID)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("accept error event %q: %w", entry.EventID, err)
		}
		insertedCount++
		insertedBytes += entry.PayloadBytes
	}
	if insertedCount > 0 {
		if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
			active_items = active_items + ?, active_bytes = active_bytes + ?, updated_at = ? WHERE id = 1`),
			insertedCount, insertedBytes, unique[0].AcceptedAt.UTC()); err != nil {
			return fmt.Errorf("reserve error Inbox capacity: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit error Inbox acceptance: %w", err)
	}
	return nil
}

type errorInboxState struct {
	activeItems int64
	activeBytes int64
}

func (s *Store) lockErrorInboxStateTx(ctx context.Context, tx *sql.Tx) (errorInboxState, error) {
	if s.dialect != dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE telemetry_inbox_state SET updated_at = updated_at WHERE id = 1`,
		); err != nil {
			return errorInboxState{}, err
		}
	}
	query := `SELECT active_items, active_bytes FROM telemetry_inbox_state WHERE id = 1`
	if s.dialect == dialect.Postgres {
		query += " FOR UPDATE"
	}
	var state errorInboxState
	if err := tx.QueryRowContext(ctx, query).Scan(&state.activeItems, &state.activeBytes); err != nil {
		return errorInboxState{}, err
	}
	return state, nil
}

func (s *Store) sourceMapSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int,
	release string,
) (json.RawMessage, error) {
	snapshot := map[string]string{"kind": "none"}
	if release == "" {
		return json.Marshal(snapshot)
	}
	query := `SELECT filename, sha256 FROM source_map_artifacts
		WHERE project_id = ? AND release = ? ORDER BY filename`
	if s.dialect == dialect.Postgres {
		query += " FOR SHARE"
	}
	rows, err := tx.QueryContext(ctx, bind(s.dialect, query), projectID, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var filename, digest string
		if err := rows.Scan(&filename, &digest); err != nil {
			return nil, err
		}
		snapshot["file:"+filename] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snapshot) > 1 {
		snapshot["kind"] = "content-addressed-v1"
		snapshot["release"] = release
	}
	return json.Marshal(snapshot)
}

func (s *Store) ClaimErrorEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.InboxItem, error) {
	if owner == "" || limit <= 0 || now.IsZero() || ttl <= 0 {
		return nil, fmt.Errorf("%w: owner, limit, current time, and TTL are required", errortrack.ErrInvalid)
	}
	if limit > 100 {
		limit = 100
	}
	if s.dialect == dialect.Postgres {
		return s.claimPostgresErrorEvents(ctx, owner, limit, now.UTC(), ttl)
	}
	return s.claimSQLiteErrorEvents(ctx, owner, limit, now.UTC(), ttl)
}

func (s *Store) claimPostgresErrorEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.InboxItem, error) {
	claimUntil := now.Add(ttl)
	query := `WITH picked AS (
		SELECT id FROM telemetry_inbox
		WHERE (status = 'pending' AND available_at <= ?)
		   OR (status = 'processing' AND claim_until <= ?)
		ORDER BY available_at, id FOR UPDATE SKIP LOCKED LIMIT ?
	), claimed AS (
		UPDATE telemetry_inbox AS item SET status = 'processing', claim_owner = ?,
			claim_until = ?, claim_epoch = claim_epoch + 1, attempts = attempts + 1, updated_at = ?
		FROM picked WHERE item.id = picked.id
		RETURNING item.id, item.project_id, item.event_id, item.payload, item.occurred_at, item.accepted_at,
			item.source_map_snapshot, item.claim_epoch, item.claim_until, item.attempts
	)
	SELECT claimed.id, claimed.project_id, projects.slug, claimed.event_id, claimed.payload,
		claimed.occurred_at, claimed.accepted_at, claimed.source_map_snapshot,
		claimed.claim_epoch, claimed.claim_until, claimed.attempts
	FROM claimed JOIN projects ON projects.id = claimed.project_id ORDER BY claimed.id`
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, query), now, now, limit, owner, claimUntil, now)
	if err != nil {
		return nil, fmt.Errorf("claim PostgreSQL error Inbox: %w", err)
	}
	defer rows.Close()
	return scanErrorInboxRows(rows)
}

func (s *Store) claimSQLiteErrorEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.InboxItem, error) {
	claimUntil := now.Add(ttl)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin SQLite error Inbox claim: %w", err)
	}
	defer tx.Rollback()
	ids, err := selectClaimableSQLiteErrorEventIDsTx(ctx, tx, now, limit)
	if err != nil {
		return nil, err
	}
	items := make([]errortrack.InboxItem, 0, len(ids))
	slugs := make(map[int]string)
	for _, id := range ids {
		query := `UPDATE telemetry_inbox SET status = 'processing', claim_owner = ?,
			claim_until = ?, claim_epoch = claim_epoch + 1, attempts = attempts + 1, updated_at = ?
			WHERE id = ? AND ((status = 'pending' AND available_at <= ?)
				OR (status = 'processing' AND claim_until <= ?))
			RETURNING id, project_id, event_id, payload, occurred_at, accepted_at,
				source_map_snapshot, claim_epoch, claim_until, attempts`
		var item errortrack.InboxItem
		var snapshot string
		if err := tx.QueryRowContext(ctx, query, owner, claimUntil, now, id, now, now).
			Scan(
				&item.ID,
				&item.ProjectID,
				&item.EventID,
				&item.Payload,
				&item.OccurredAt,
				&item.AcceptedAt,
				&snapshot,
				&item.ClaimEpoch,
				&item.ClaimUntil,
				&item.Attempts,
			); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("claim SQLite error Inbox item %d: %w", id, err)
		}
		item.SourceMapSnapshot = json.RawMessage(snapshot)
		slug, cached := slugs[item.ProjectID]
		if !cached {
			if err := tx.QueryRowContext(ctx, `SELECT slug FROM projects WHERE id = ?`, item.ProjectID).
				Scan(&slug); err != nil {
				return nil, fmt.Errorf("read error Inbox project %d: %w", item.ProjectID, err)
			}
			slugs[item.ProjectID] = slug
		}
		item.ProjectSlug = slug
		items = append(items, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite error Inbox claim: %w", err)
	}
	return items, nil
}

func (s *Store) CompleteErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	delta errortrack.IssueDelta,
	processedAt time.Time,
) ([]errortrack.IssueChange, error) {
	if id <= 0 || owner == "" || claimEpoch <= 0 || delta.ProjectID <= 0 || delta.LastEventID == "" ||
		processedAt.IsZero() {
		return nil, fmt.Errorf("%w: incomplete completion input", errortrack.ErrInvalid)
	}
	res, err := s.completeErrorEvents(
		ctx,
		owner,
		[]errortrack.ErrorCompletion{{
			ID:          id,
			EventID:     delta.LastEventID,
			ClaimEpoch:  claimEpoch,
			Delta:       delta,
			ProcessedAt: processedAt,
		}},
		true,
	)
	if err != nil {
		return nil, err
	}
	return res.Changes, nil
}

// CompleteErrorEvents atomically completes a batch of claimed error Inbox
// items in one transaction respecting canonical lock ordering:
// telemetry_inbox_state(id=1) -> telemetry_inbox(id ASC) -> issues(project_id, fingerprint).
func (s *Store) CompleteErrorEvents(
	ctx context.Context,
	owner string,
	completions []errortrack.ErrorCompletion,
) (errortrack.BatchCompletionResult, error) {
	return s.completeErrorEvents(ctx, owner, completions, false)
}

func (s *Store) completeErrorEvents(
	ctx context.Context,
	owner string,
	completions []errortrack.ErrorCompletion,
	failOnClaimLost bool,
) (errortrack.BatchCompletionResult, error) {
	if owner == "" {
		return errortrack.BatchCompletionResult{}, fmt.Errorf("%w: incomplete completion input", errortrack.ErrInvalid)
	}
	if len(completions) == 0 {
		return errortrack.BatchCompletionResult{}, nil
	}
	ordered := make([]errortrack.ErrorCompletion, len(completions))
	seenIDs := make(map[int64]struct{}, len(completions))
	for idx, comp := range completions {
		if comp.ID <= 0 || comp.ClaimEpoch <= 0 || comp.Delta.ProjectID <= 0 || comp.Delta.LastEventID == "" ||
			comp.ProcessedAt.IsZero() {
			return errortrack.BatchCompletionResult{}, fmt.Errorf(
				"%w: incomplete completion input",
				errortrack.ErrInvalid,
			)
		}
		if _, dup := seenIDs[comp.ID]; dup {
			return errortrack.BatchCompletionResult{}, fmt.Errorf(
				"%w: duplicate completion item %d",
				errortrack.ErrInvalid,
				comp.ID,
			)
		}
		seenIDs[comp.ID] = struct{}{}
		if comp.EventID == "" {
			comp.EventID = comp.Delta.LastEventID
		}
		comp.ProcessedAt = comp.ProcessedAt.UTC()
		ordered[idx] = comp
	}
	if len(ordered) > 1 {
		sort.Slice(ordered, func(i, j int) bool {
			return ordered[i].ID < ordered[j].ID
		})
	}

	if s.dialect != dialect.Postgres {
		s.issueMu.Lock()
		defer s.issueMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errortrack.BatchCompletionResult{}, fmt.Errorf("begin error Inbox completion: %w", err)
	}
	defer tx.Rollback()

	// Canonical lock order step 1: lock telemetry_inbox_state(id=1) before
	// touching telemetry_inbox or issues to match Accept/Retry/Terminal/DeleteProject.
	state, err := s.lockErrorInboxStateTx(ctx, tx)
	if err != nil {
		return errortrack.BatchCompletionResult{}, fmt.Errorf("lock error Inbox completion capacity: %w", err)
	}

	readQuery := `SELECT project_id, event_id, payload_bytes, accepted_at FROM telemetry_inbox
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`
	if s.dialect == dialect.Postgres {
		readQuery += " FOR UPDATE"
	}
	readQuery = bind(s.dialect, readQuery)

	type verifiedCompletion struct {
		comp         errortrack.ErrorCompletion
		payloadBytes int64
	}
	valid := make([]verifiedCompletion, 0, len(ordered))
	result := errortrack.BatchCompletionResult{
		CompletedIDs: make([]int64, 0, len(ordered)),
	}
	var totalPayloadBytes int64
	var batchUpdatedAt time.Time

	// Canonical lock order step 2: lock/verify claimed telemetry_inbox rows in
	// deterministic ascending ID order.
	for _, comp := range ordered {
		var projectID int
		var eventID string
		var payloadBytes int64
		var acceptedAt time.Time
		if err := tx.QueryRowContext(ctx, readQuery, comp.ID, owner, comp.ClaimEpoch).
			Scan(&projectID, &eventID, &payloadBytes, &acceptedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				if failOnClaimLost {
					return errortrack.BatchCompletionResult{}, errortrack.ErrClaimLost
				}
				slog.Warn(
					"skip error Inbox completion after losing claim lease",
					"item_id", comp.ID,
					"event_id", comp.EventID,
					"project_id", comp.Delta.ProjectID,
					"owner", owner,
					"claim_epoch", comp.ClaimEpoch,
					"attempts", comp.Attempts,
					"error", errortrack.ErrClaimLost,
				)
				result.LostIDs = append(result.LostIDs, comp.ID)
				continue
			}
			return errortrack.BatchCompletionResult{}, fmt.Errorf("read error Inbox claim %d: %w", comp.ID, err)
		}
		if projectID != comp.Delta.ProjectID || eventID != comp.Delta.LastEventID {
			return errortrack.BatchCompletionResult{}, fmt.Errorf(
				"%w: completion identity mismatch",
				errortrack.ErrInvalid,
			)
		}
		comp.Delta.AcceptedAt = acceptedAt.UTC()
		comp.Delta.AppliedAt = comp.ProcessedAt
		valid = append(valid, verifiedCompletion{comp: comp, payloadBytes: payloadBytes})
		totalPayloadBytes += payloadBytes
		if batchUpdatedAt.IsZero() || comp.ProcessedAt.After(batchUpdatedAt) {
			batchUpdatedAt = comp.ProcessedAt
		}
	}
	if len(valid) == 0 {
		return result, nil
	}
	if state.activeItems < int64(len(valid)) || state.activeBytes < totalPayloadBytes {
		return errortrack.BatchCompletionResult{}, errors.New("error Inbox capacity counters are inconsistent")
	}

	type mergedDeltaEntry struct {
		delta     errortrack.IssueDelta
		repItemID int64
	}
	mergedKeys := make([]issueKey, 0, len(valid))
	mergedByKey := make(map[issueKey]*mergedDeltaEntry, len(valid))
	for _, v := range valid {
		d := v.comp.Delta
		key := issueKey{projectID: d.ProjectID, fingerprint: d.Fingerprint}
		cur, exists := mergedByKey[key]
		if !exists {
			mergedKeys = append(mergedKeys, key)
			mergedByKey[key] = &mergedDeltaEntry{delta: d, repItemID: v.comp.ID}
			continue
		}
		cur.delta.Count += d.Count
		cur.delta.SessionBits |= d.SessionBits
		if cur.delta.FirstSeen.IsZero() || (!d.FirstSeen.IsZero() && d.FirstSeen.Before(cur.delta.FirstSeen)) {
			cur.delta.FirstSeen = d.FirstSeen
		}
		if !d.LastSeen.Before(cur.delta.LastSeen) {
			cur.delta.LastSeen = d.LastSeen
			cur.delta.LastEventID = d.LastEventID
			cur.delta.Title = d.Title
			cur.delta.Level = d.Level
		}
		if d.AcceptedAt.After(cur.delta.AcceptedAt) {
			cur.delta.AcceptedAt = d.AcceptedAt
			cur.repItemID = v.comp.ID
		}
		if d.AppliedAt.Before(cur.delta.AppliedAt) {
			cur.delta.AppliedAt = d.AppliedAt
		}
	}
	mergedDeltas := make([]errortrack.IssueDelta, 0, len(mergedKeys))
	for _, key := range mergedKeys {
		mergedDeltas = append(mergedDeltas, mergedByKey[key].delta)
	}

	// Canonical lock order step 3: apply merged issue deltas (internally sorted
	// by project_id, fingerprint) and receive both lifecycle changes and issue IDs.
	changes, issueIDs, err := s.applyIssueDeltasWithIDsTx(ctx, tx, mergedDeltas)
	if err != nil {
		return errortrack.BatchCompletionResult{}, fmt.Errorf("apply accepted error events: %w", err)
	}

	issueIDToKey := make(map[int]issueKey, len(issueIDs))
	for key, id := range issueIDs {
		issueIDToKey[id] = key
	}
	changeByItemID := make(map[int64]*errortrack.IssueChange, len(changes))
	if len(changes) > 0 {
		insertLifecycleQuery := bind(s.dialect, `INSERT INTO issue_lifecycle_events(
			project_id, issue_id, inbox_item_id, kind, title, level, seen_at,
			status, available_at, claim_owner, claim_epoch, attempts, last_error, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, '', 0, 0, '', ?, ?)`)
		for idx := range changes {
			change := changes[idx]
			key := issueIDToKey[change.IssueID]
			inboxItemID := mergedByKey[key].repItemID
			if _, err := tx.ExecContext(
				ctx,
				insertLifecycleQuery,
				change.ProjectID,
				change.IssueID,
				inboxItemID,
				change.Kind,
				change.Title,
				change.Level,
				change.SeenAt.UTC(),
				batchUpdatedAt,
				batchUpdatedAt,
				batchUpdatedAt,
			); err != nil {
				return errortrack.BatchCompletionResult{}, fmt.Errorf(
					"persist issue lifecycle event for item %d: %w",
					inboxItemID,
					err,
				)
			}
			c := change
			changeByItemID[inboxItemID] = &c
		}
	}

	updateInboxQuery := bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'completed', payload = NULL, payload_bytes = 0, claim_owner = '', claim_until = NULL,
		outcome = ?, processed_at = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`)
	for _, v := range valid {
		key := issueKey{projectID: v.comp.Delta.ProjectID, fingerprint: v.comp.Delta.Fingerprint}
		issueID := issueIDs[key]
		var lifecycle *errortrack.IssueChange
		lifecycleKind := ""
		if ch := changeByItemID[v.comp.ID]; ch != nil {
			lifecycle = ch
			lifecycleKind = ch.Kind
		}
		outcome, err := json.Marshal(struct {
			Fingerprint   string                  `json:"fingerprint"`
			IssueID       int                     `json:"issue_id"`
			LifecycleKind string                  `json:"lifecycle_kind,omitempty"`
			Lifecycle     *errortrack.IssueChange `json:"lifecycle,omitempty"`
		}{
			Fingerprint:   v.comp.Delta.Fingerprint,
			IssueID:       issueID,
			LifecycleKind: lifecycleKind,
			Lifecycle:     lifecycle,
		})
		if err != nil {
			return errortrack.BatchCompletionResult{}, fmt.Errorf(
				"encode completed error event outcome %q: %w",
				v.comp.EventID,
				err,
			)
		}
		execRes, err := tx.ExecContext(
			ctx,
			updateInboxQuery,
			string(outcome),
			v.comp.ProcessedAt,
			v.comp.ProcessedAt,
			v.comp.ProcessedAt,
			v.comp.ID,
			owner,
			v.comp.ClaimEpoch,
		)
		if err != nil {
			return errortrack.BatchCompletionResult{}, fmt.Errorf("complete error Inbox item %d: %w", v.comp.ID, err)
		}
		updated, err := execRes.RowsAffected()
		if err != nil {
			return errortrack.BatchCompletionResult{}, err
		}
		if updated != 1 {
			return errortrack.BatchCompletionResult{}, errortrack.ErrClaimLost
		}
		result.CompletedIDs = append(result.CompletedIDs, v.comp.ID)
	}

	if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
		active_items = active_items - ?, active_bytes = active_bytes - ?, updated_at = ? WHERE id = 1`),
		len(valid), totalPayloadBytes, batchUpdatedAt); err != nil {
		return errortrack.BatchCompletionResult{}, fmt.Errorf("release completed error Inbox capacity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return errortrack.BatchCompletionResult{}, fmt.Errorf("commit error Inbox batch completion: %w", err)
	}
	result.Changes = changes
	return result, nil
}

// IssueIDForErrorEvent resolves the stable event-to-Issue relation retained in
// a completed durable acceptance receipt. Replay uses it to turn an error
// marker's event_id into an exact product link without guessing from the
// Issue's mutable last_event_id.
func (s *Store) IssueIDForErrorEvent(ctx context.Context, projectID int, eventID string) (int, bool, error) {
	var outcome string
	err := s.db.QueryRowContext(
		ctx,
		bind(s.dialect, `SELECT outcome FROM telemetry_inbox
			WHERE project_id = ? AND event_id = ? AND status = 'completed'`),
		projectID,
		eventID,
	).Scan(&outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	issueID, ok := parseCompletedReceiptIssueID(outcome)
	return issueID, ok, nil
}

func parseCompletedReceiptIssueID(outcome string) (int, bool) {
	if outcome == "" {
		return 0, false
	}
	var receipt struct {
		IssueID int `json:"issue_id"`
	}
	if json.Unmarshal([]byte(outcome), &receipt) != nil || receipt.IssueID <= 0 {
		return 0, false
	}
	return receipt.IssueID, true
}

func (s *Store) RetryErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	retriedAt time.Time,
	availableAt time.Time,
	reason string,
) error {
	if id <= 0 || owner == "" || claimEpoch <= 0 {
		return fmt.Errorf("%w: incomplete retry input", errortrack.ErrInvalid)
	}
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	retriedAt = retriedAt.UTC()
	availableAt = availableAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin error Inbox retry: %w", err)
	}
	defer tx.Rollback()
	// Always lock telemetry_inbox_state(id=1) before modifying telemetry_inbox
	// to match Accept/Complete/Terminal/DeleteProject lock ordering and prevent
	// PostgreSQL AB-BA deadlocks (SQLSTATE 40P01).
	if _, err := s.lockErrorInboxStateTx(ctx, tx); err != nil {
		return fmt.Errorf("lock error Inbox retry state: %w", err)
	}
	result, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'pending', available_at = ?, claim_owner = '', claim_until = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`),
		availableAt, reason, retriedAt, id, owner, claimEpoch)
	if err != nil {
		return fmt.Errorf("retry error Inbox item %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
		retries_total = retries_total + 1, updated_at = ? WHERE id = 1`), retriedAt); err != nil {
		return fmt.Errorf("record error Inbox retry %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit error Inbox retry %d: %w", id, err)
	}
	return nil
}

func (s *Store) ReleaseErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	releasedAt time.Time,
) error {
	if id <= 0 || owner == "" || claimEpoch <= 0 {
		return fmt.Errorf("%w: incomplete release input", errortrack.ErrInvalid)
	}
	releasedAt = releasedAt.UTC()
	result, err := s.db.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'pending', available_at = ?, claim_owner = '', claim_until = NULL,
		attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
		last_error = 'worker_shutdown: released', updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`),
		releasedAt, releasedAt, id, owner, claimEpoch)
	if err != nil {
		return fmt.Errorf("release error Inbox item %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	return nil
}

func (s *Store) TerminalErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	completedAt time.Time,
	attempts int,
	reason string,
) error {
	if id <= 0 || owner == "" || claimEpoch <= 0 || attempts <= 0 {
		return fmt.Errorf("%w: terminal owner, epoch, and attempt count are required", errortrack.ErrInvalid)
	}
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	completedAt = completedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin terminal error Inbox item: %w", err)
	}
	defer tx.Rollback()
	state, err := s.lockErrorInboxStateTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("lock terminal error Inbox capacity: %w", err)
	}
	query := `SELECT payload_bytes FROM telemetry_inbox
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`
	if s.dialect == dialect.Postgres {
		query += " FOR UPDATE"
	}
	var payloadBytes int64
	if err := tx.QueryRowContext(ctx, bind(s.dialect, query), id, owner, claimEpoch).Scan(&payloadBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errortrack.ErrClaimLost
		}
		return fmt.Errorf("read terminal error Inbox item %d: %w", id, err)
	}
	if state.activeItems < 1 || state.activeBytes < payloadBytes {
		return errors.New("error Inbox capacity counters are inconsistent")
	}
	outcome, _ := json.Marshal(map[string]string{"status": "terminal", "reason": reason})
	result, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'terminal', payload = NULL, payload_bytes = 0, claim_owner = '', claim_until = NULL,
		attempts = ?, last_error = ?, outcome = ?, processed_at = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`),
		attempts, reason, string(outcome), completedAt, completedAt, completedAt, id, owner, claimEpoch)
	if err != nil {
		return fmt.Errorf("mark terminal error Inbox item %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
		active_items = active_items - 1, active_bytes = active_bytes - ?,
		terminal_failures_total = terminal_failures_total + 1, drops_total = drops_total + 1, updated_at = ?
		WHERE id = 1`), payloadBytes, completedAt); err != nil {
		return fmt.Errorf("record terminal error Inbox item %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit terminal error Inbox item %d: %w", id, err)
	}
	return nil
}

// ErrorInboxSummary reads the O(1) singleton summary row from
// telemetry_inbox_state without scanning telemetry_inbox.
func (s *Store) ErrorInboxSummary(ctx context.Context) (errortrack.InboxSummary, error) {
	var summary errortrack.InboxSummary
	if err := s.db.QueryRowContext(ctx, `SELECT active_items, active_bytes, rejections_total, retries_total,
		terminal_failures_total, drops_total FROM telemetry_inbox_state WHERE id = 1`).Scan(
		&summary.ActiveItems,
		&summary.ActiveBytes,
		&summary.RejectionsTotal,
		&summary.RetriesTotal,
		&summary.TerminalFailuresTotal,
		&summary.DropsTotal,
	); err != nil {
		return errortrack.InboxSummary{}, fmt.Errorf("read error Inbox summary: %w", err)
	}
	return summary, nil
}

// OldestActiveErrorAcceptedAt returns the earliest accepted_at among active
// (pending or processing) Inbox entries using the (status, accepted_at, id)
// index for each active status.
func (s *Store) OldestActiveErrorAcceptedAt(ctx context.Context) (*time.Time, error) {
	var oldest *time.Time
	for _, status := range []string{"pending", "processing"} {
		var candidate time.Time
		err := s.db.QueryRowContext(ctx, bind(s.dialect, `SELECT accepted_at FROM telemetry_inbox
			WHERE status = ? ORDER BY accepted_at, id LIMIT 1`), status).Scan(&candidate)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read oldest %s error Inbox item: %w", status, err)
		}
		if oldest == nil || candidate.Before(*oldest) {
			t := candidate.UTC()
			oldest = &t
		}
	}
	return oldest, nil
}

func (s *Store) ErrorInboxDiagnostics(ctx context.Context, recentLimit int) (errortrack.InboxDiagnostics, error) {
	if recentLimit <= 0 || recentLimit > 100 {
		recentLimit = 20
	}
	summary, err := s.ErrorInboxSummary(ctx)
	if err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("read error Inbox diagnostics: %w", err)
	}
	diagnostics := errortrack.InboxDiagnostics{
		ActiveItems:           summary.ActiveItems,
		ActiveBytes:           summary.ActiveBytes,
		RejectionsTotal:       summary.RejectionsTotal,
		RetriesTotal:          summary.RetriesTotal,
		TerminalFailuresTotal: summary.TerminalFailuresTotal,
		DropsTotal:            summary.DropsTotal,
	}
	// Count each active status via the (status, ...) index instead of scanning
	// historical completed/terminal rows across the entire telemetry_inbox table.
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telemetry_inbox WHERE status = 'pending'`).
		Scan(&diagnostics.PendingItems); err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("read pending error Inbox count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telemetry_inbox WHERE status = 'processing'`).
		Scan(&diagnostics.ProcessingItems); err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("read processing error Inbox count: %w", err)
	}
	oldest, err := s.OldestActiveErrorAcceptedAt(ctx)
	if err != nil {
		return errortrack.InboxDiagnostics{}, err
	}
	diagnostics.OldestAcceptedAt = oldest
	diagnostics.RecentTerminal = make([]errortrack.TerminalDiagnostic, 0)
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, `SELECT project_id, event_id, attempts, payload_bytes,
		last_error, completed_at FROM telemetry_inbox WHERE status = 'terminal'
		ORDER BY completed_at DESC, id DESC LIMIT ?`), recentLimit)
	if err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("list terminal error Inbox diagnostics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item errortrack.TerminalDiagnostic
		if err := rows.Scan(
			&item.ProjectID,
			&item.EventID,
			&item.Attempts,
			&item.PayloadBytes,
			&item.LastError,
			&item.CompletedAt,
		); err != nil {
			return errortrack.InboxDiagnostics{}, fmt.Errorf("scan terminal error Inbox diagnostic: %w", err)
		}
		diagnostics.RecentTerminal = append(diagnostics.RecentTerminal, item)
	}
	if err := rows.Err(); err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("iterate terminal error Inbox diagnostics: %w", err)
	}
	return diagnostics, nil
}

func (s *Store) ClaimIssueLifecycleEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.LifecycleEvent, error) {
	if owner == "" || limit <= 0 || now.IsZero() || ttl <= 0 {
		return nil, fmt.Errorf("%w: owner, limit, current time, and TTL are required", errortrack.ErrInvalid)
	}
	if limit > 100 {
		limit = 100
	}
	now = now.UTC()
	claimUntil := now.Add(ttl)
	if s.dialect == dialect.Postgres {
		query := `WITH picked AS (
			SELECT id FROM issue_lifecycle_events
			WHERE (status = 'pending' AND available_at <= ?)
			   OR (status = 'processing' AND claim_until <= ?)
			ORDER BY available_at, id FOR UPDATE SKIP LOCKED LIMIT ?
		)
		UPDATE issue_lifecycle_events AS item SET status = 'processing', claim_owner = ?,
			claim_until = ?, claim_epoch = claim_epoch + 1, attempts = attempts + 1, updated_at = ?
		FROM picked WHERE item.id = picked.id
		RETURNING item.id, item.project_id, item.issue_id, item.inbox_item_id, item.kind,
			item.title, item.level, item.seen_at, item.claim_epoch, item.claim_until, item.attempts`
		rows, err := s.db.QueryContext(ctx, bind(s.dialect, query), now, now, limit, owner, claimUntil, now)
		if err != nil {
			return nil, fmt.Errorf("claim PostgreSQL issue lifecycle events: %w", err)
		}
		defer rows.Close()
		return scanLifecycleEventRows(rows)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin SQLite issue lifecycle claim: %w", err)
	}
	defer tx.Rollback()
	ids, err := selectClaimableSQLiteLifecycleEventIDsTx(ctx, tx, now, limit)
	if err != nil {
		return nil, err
	}
	events := make([]errortrack.LifecycleEvent, 0, len(ids))
	for _, id := range ids {
		var evt errortrack.LifecycleEvent
		err := tx.QueryRowContext(ctx, `UPDATE issue_lifecycle_events SET status = 'processing', claim_owner = ?,
			claim_until = ?, claim_epoch = claim_epoch + 1, attempts = attempts + 1, updated_at = ?
			WHERE id = ? AND ((status = 'pending' AND available_at <= ?)
				OR (status = 'processing' AND claim_until <= ?))
			RETURNING id, project_id, issue_id, inbox_item_id, kind, title, level, seen_at,
				claim_epoch, claim_until, attempts`,
			owner, claimUntil, now, id, now, now).Scan(
			&evt.ID,
			&evt.ProjectID,
			&evt.IssueID,
			&evt.InboxItemID,
			&evt.Kind,
			&evt.Title,
			&evt.Level,
			&evt.SeenAt,
			&evt.ClaimEpoch,
			&evt.ClaimUntil,
			&evt.Attempts,
		)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("claim SQLite issue lifecycle event %d: %w", id, err)
		}
		events = append(events, evt)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite issue lifecycle claim: %w", err)
	}
	return events, nil
}

func (s *Store) CompleteIssueLifecycleEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	dispatchedAt time.Time,
) error {
	dispatchedAt = dispatchedAt.UTC()
	result, err := s.db.ExecContext(ctx, bind(s.dialect, `UPDATE issue_lifecycle_events SET
		status = 'dispatched', claim_owner = '', claim_until = NULL, dispatched_at = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`),
		dispatchedAt, dispatchedAt, id, owner, claimEpoch)
	if err != nil {
		return fmt.Errorf("complete issue lifecycle event %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	return nil
}

func (s *Store) RetryIssueLifecycleEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	retriedAt time.Time,
	availableAt time.Time,
	reason string,
) error {
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	result, err := s.db.ExecContext(ctx, bind(s.dialect, `UPDATE issue_lifecycle_events SET
		status = 'pending', available_at = ?, claim_owner = '', claim_until = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`),
		availableAt.UTC(), reason, retriedAt.UTC(), id, owner, claimEpoch)
	if err != nil {
		return fmt.Errorf("retry issue lifecycle event %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	return nil
}

func (s *Store) ReleaseIssueLifecycleEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	releasedAt time.Time,
) error {
	releasedAt = releasedAt.UTC()
	result, err := s.db.ExecContext(ctx, bind(s.dialect, `UPDATE issue_lifecycle_events SET
		status = 'pending', available_at = ?, claim_owner = '', claim_until = NULL,
		attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
		last_error = 'worker_shutdown: released', updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`),
		releasedAt, releasedAt, id, owner, claimEpoch)
	if err != nil {
		return fmt.Errorf("release issue lifecycle event %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	return nil
}

func (s *Store) FailIssueLifecycleEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	failedAt time.Time,
	reason string,
) error {
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	failedAt = failedAt.UTC()
	result, err := s.db.ExecContext(ctx, bind(s.dialect, `UPDATE issue_lifecycle_events SET
		status = 'failed', claim_owner = '', claim_until = NULL, last_error = ?, dispatched_at = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ? AND claim_epoch = ?`),
		reason, failedAt, failedAt, id, owner, claimEpoch)
	if err != nil {
		return fmt.Errorf("fail issue lifecycle event %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	return nil
}

func (s *Store) DeleteCompletedErrorReceipts(ctx context.Context, cutoff time.Time) (int, error) {
	cutoff = cutoff.UTC().Round(0)
	deleted, err := s.deleteInBatches(
		ctx,
		"completed error receipts",
		`DELETE FROM telemetry_inbox WHERE id IN (
			SELECT id FROM telemetry_inbox
			WHERE status IN ('completed', 'terminal') AND completed_at < ?
			ORDER BY id LIMIT ?
		)`,
		cutoff,
	)
	if err != nil {
		return deleted, err
	}
	if _, err := s.deleteInBatches(
		ctx,
		"completed issue lifecycle events",
		`DELETE FROM issue_lifecycle_events WHERE id IN (
			SELECT id FROM issue_lifecycle_events
			WHERE status IN ('dispatched', 'failed') AND dispatched_at < ?
			ORDER BY id LIMIT ?
		)`,
		cutoff,
	); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func selectClaimableSQLiteErrorEventIDsTx(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	limit int,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM telemetry_inbox
		WHERE (status = 'pending' AND available_at <= ?)
		   OR (status = 'processing' AND claim_until <= ?)
		ORDER BY available_at, id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select SQLite error Inbox: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan SQLite error Inbox ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SQLite error Inbox IDs: %w", err)
	}
	return ids, nil
}

func selectClaimableSQLiteLifecycleEventIDsTx(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	limit int,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM issue_lifecycle_events
		WHERE (status = 'pending' AND available_at <= ?)
		   OR (status = 'processing' AND claim_until <= ?)
		ORDER BY available_at, id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select SQLite issue lifecycle events: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func scanErrorInboxRows(rows *sql.Rows) ([]errortrack.InboxItem, error) {
	var items []errortrack.InboxItem
	for rows.Next() {
		var item errortrack.InboxItem
		var snapshot string
		if err := rows.Scan(
			&item.ID,
			&item.ProjectID,
			&item.ProjectSlug,
			&item.EventID,
			&item.Payload,
			&item.OccurredAt,
			&item.AcceptedAt,
			&snapshot,
			&item.ClaimEpoch,
			&item.ClaimUntil,
			&item.Attempts,
		); err != nil {
			return nil, fmt.Errorf("scan error Inbox item: %w", err)
		}
		item.SourceMapSnapshot = json.RawMessage(snapshot)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate error Inbox items: %w", err)
	}
	return items, nil
}

func scanLifecycleEventRows(rows *sql.Rows) ([]errortrack.LifecycleEvent, error) {
	var events []errortrack.LifecycleEvent
	for rows.Next() {
		var evt errortrack.LifecycleEvent
		if err := rows.Scan(
			&evt.ID,
			&evt.ProjectID,
			&evt.IssueID,
			&evt.InboxItemID,
			&evt.Kind,
			&evt.Title,
			&evt.Level,
			&evt.SeenAt,
			&evt.ClaimEpoch,
			&evt.ClaimUntil,
			&evt.Attempts,
		); err != nil {
			return nil, fmt.Errorf("scan issue lifecycle event: %w", err)
		}
		events = append(events, evt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue lifecycle events: %w", err)
	}
	return events, nil
}
