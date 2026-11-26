package meta

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
)

// Concurrent first spikes on a project with no platform_alert_states row yet
// (issue #387): every record must commit its buckets, and exactly one of them
// may report the firing transition of cycle 1.
func TestRecordPlatformErrorRateConcurrentFirstSpike(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Race First Spike", "race-first-spike")
		if err != nil {
			t.Fatal(err)
		}
		const (
			workers   = 16
			rounds    = 5
			batchSize = 3
			threshold = 1 // per minute: limit is 5 over the 5-minute window
		)
		window := 5 * time.Minute
		now := time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)

		var (
			mu       sync.Mutex
			firings  []PlatformErrorSpikeTransition
			errs     []error
			wg       sync.WaitGroup
			start    = make(chan struct{})
			expected int
		)
		for w := range workers {
			// Each worker writes its own bucket so the bucket upserts do not
			// serialize the workers before they reach the state row.
			occurredAt := now.Add(-time.Duration(w) * platformErrorRateBucketSeconds * time.Second)
			perRound := 1
			if w%2 == 1 {
				perRound = batchSize
			}
			expected += rounds * perRound
			wg.Go(func() {
				<-start
				for range rounds {
					var got []PlatformErrorSpikeTransition
					var err error
					if perRound == 1 {
						var tr PlatformErrorSpikeTransition
						tr, err = store.RecordPlatformErrorRate(ctx, project.ID, occurredAt, now, window, threshold)
						got = []PlatformErrorSpikeTransition{tr}
					} else {
						occurrences := make([]time.Time, perRound)
						for i := range occurrences {
							occurrences[i] = occurredAt
						}
						var results []PlatformErrorRateBatchResult
						results, err = store.RecordPlatformErrorRates(ctx, now, window, []PlatformErrorRateBatch{{
							ProjectID:       project.ID,
							ThresholdPerMin: threshold,
							OccurredAt:      occurrences,
						}})
						for _, res := range results {
							got = append(got, res.Transitions...)
						}
					}
					mu.Lock()
					if err != nil {
						errs = append(errs, err)
					}
					for _, tr := range got {
						if tr.Transition != "" {
							firings = append(firings, tr)
						}
					}
					mu.Unlock()
				}
			})
		}
		close(start)
		wg.Wait()

		for _, err := range errs {
			t.Errorf("concurrent record failed: %v", err)
		}
		if len(firings) != 1 || firings[0].Transition != platformAlertStatusFiring || firings[0].Cycle != 1 {
			t.Fatalf("transitions=%#v want exactly one firing of cycle 1", firings)
		}
		total := 0
		for _, row := range platformBucketRows(t, store, project.ID) {
			total += row.Count
		}
		if total != expected {
			t.Fatalf("bucket total=%d want %d (one per recorded occurrence)", total, expected)
		}
		state := platformSpikeStateRow(t, store, project.ID)
		if state == nil || state.Status != platformAlertStatusFiring || state.Cycle != 1 ||
			!state.StartsAt.Equal(firings[0].StartsAt) {
			t.Fatalf("state=%#v want firing cycle 1 starting at %v", state, firings[0].StartsAt)
		}
	})
}

// A record that saw no state row but loses the first insert to a concurrent
// transaction must re-read the committed winner row and replay its own
// occurrences on top of it, keeping its bucket increments and never repeating
// the winner's transition. PostgreSQL only: SQLite serializes the whole write
// transaction, so its insert never races.
func TestRecordPlatformErrorRateLosesFirstStateInsert(t *testing.T) {
	window := 5 * time.Minute
	now := time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)
	winnerStart := now.Add(-time.Minute)
	cases := []struct {
		name            string
		winnerStatus    string
		winnerEvents    int
		wantTransitions []PlatformErrorSpikeTransition
		wantCycle       int64
		wantStart       time.Time
	}{
		// The winner already fired cycle 1 with its own spike committed: the
		// loser keeps it firing silently.
		{
			name:         "winner firing",
			winnerStatus: platformAlertStatusFiring,
			winnerEvents: 6,
			wantCycle:    1,
			wantStart:    winnerStart,
		},
		// The winner's row is resolved cycle 1: the loser's spike opens cycle 2.
		{
			name:         "winner resolved",
			winnerStatus: platformAlertStatusResolved,
			wantTransitions: []PlatformErrorSpikeTransition{
				{Transition: platformAlertStatusFiring, Cycle: 2, StartsAt: now, Count: 6},
			},
			wantCycle: 2,
			wantStart: now,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, store *Store) {
				if store.dialect != dialect.Postgres {
					t.Skip("only PostgreSQL runs concurrent metadata write transactions")
				}
				ctx := context.Background()
				project, err := store.CreateProject(ctx, "Race Loser", "race-loser")
				if err != nil {
					t.Fatal(err)
				}

				winner, err := store.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer winner.Rollback()
				var endsAt any
				if tc.winnerStatus == platformAlertStatusResolved {
					endsAt = winnerStart.Add(30 * time.Second)
				}
				if _, err := winner.ExecContext(
					ctx,
					`INSERT INTO platform_alert_states(project_id, kind, status, active_cycle, starts_at, ends_at, updated_at)
					 VALUES ($1, $2, $3, 1, $4, $5, $4)`,
					project.ID,
					platformAlertKindErrorRateSpike,
					tc.winnerStatus,
					winnerStart,
					endsAt,
				); err != nil {
					t.Fatal(err)
				}
				// The winner's own occurrences live in an older bucket of the window,
				// so the loser's bucket upsert does not wait on the winner.
				winnerBucket := alignPlatformErrorBucket(winnerStart)
				if tc.winnerEvents > 0 {
					if _, err := winner.ExecContext(
						ctx,
						`INSERT INTO platform_error_rate_buckets(project_id, bucket_epoch, count) VALUES ($1, $2, $3)`,
						project.ID, winnerBucket, tc.winnerEvents,
					); err != nil {
						t.Fatal(err)
					}
				}

				type outcome struct {
					results []PlatformErrorRateBatchResult
					err     error
				}
				done := make(chan outcome, 1)
				go func() {
					// Six occurrences against threshold 1/min cross the 5-minute
					// limit of 5 within this batch alone.
					occurrences := make([]time.Time, 6)
					for i := range occurrences {
						occurrences[i] = now.Add(-time.Second)
					}
					results, err := store.RecordPlatformErrorRates(ctx, now, window, []PlatformErrorRateBatch{{
						ProjectID: project.ID, ThresholdPerMin: 1, OccurredAt: occurrences,
					}})
					done <- outcome{results: results, err: err}
				}()

				waitForBlockedStateInsert(t, store, done)
				if err := winner.Commit(); err != nil {
					t.Fatal(err)
				}
				got := <-done
				if got.err != nil {
					t.Fatalf("losing record failed: %v", got.err)
				}
				want := []PlatformErrorRateBatchResult{{
					ProjectID:   project.ID,
					Count:       tc.winnerEvents + 6,
					Status:      platformAlertStatusFiring,
					Cycle:       tc.wantCycle,
					StartsAt:    tc.wantStart,
					Transitions: tc.wantTransitions,
				}}
				if !reflect.DeepEqual(got.results, want) {
					t.Fatalf("loser results=%#v want %#v", got.results, want)
				}
				// The lost first insert attempt logs nothing; only the retry's
				// transitions reach platform_alert_transitions (issue #386).
				logged := pendingTransitions(t, store, project.ID)
				if len(logged) != len(tc.wantTransitions) {
					t.Fatalf("logged transitions=%#v want %#v", logged, tc.wantTransitions)
				}
				for i, tr := range tc.wantTransitions {
					if logged[i].Status != tr.Transition || logged[i].Cycle != tr.Cycle || logged[i].Count != tr.Count {
						t.Fatalf("logged transition %d=%#v want %#v", i, logged[i], tr)
					}
				}
				for _, row := range platformBucketRows(t, store, project.ID) {
					want := tc.winnerEvents
					if row.Epoch != winnerBucket {
						want = 6
					}
					if row.Count != want {
						t.Fatalf(
							"bucket %d count=%d want %d: each side writes its occurrences once",
							row.Epoch,
							row.Count,
							want,
						)
					}
				}
				state := platformSpikeStateRow(t, store, project.ID)
				if state == nil || state.Status != platformAlertStatusFiring || state.Cycle != tc.wantCycle ||
					!state.StartsAt.Equal(tc.wantStart) || state.EndsAt != nil {
					t.Fatalf("state=%#v want firing cycle %d starting at %v", state, tc.wantCycle, tc.wantStart)
				}
			})
		})
	}
}

// waitForBlockedStateInsert waits until a backend of the test database is
// blocked on a lock while inserting into platform_alert_states, failing fast
// if the record under test returned before it got there.
func waitForBlockedStateInsert[T any](t *testing.T, store *Store, done <-chan T) {
	t.Helper()
	waitForBlockedQuery(t, store, done, `INSERT INTO platform_alert_states%`)
}

// waitForBlockedQuery waits until a backend of the test database is blocked
// on a lock while running a statement matching the SQL LIKE pattern, failing
// fast if the call under test returned before it got there.
func waitForBlockedQuery[T any](t *testing.T, store *Store, done <-chan T, pattern string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case out := <-done:
			t.Fatalf("call returned before blocking on %q: %#v", pattern, out)
		default:
		}
		var blocked int
		if err := store.db.QueryRowContext(
			context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`,
			pattern,
		).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("call never blocked on a lock running %q", pattern)
}

// seedPlatformErrorSpike commits a project's error_rate_spike state row and,
// when count > 0, one in-window bucket holding count occurrences.
func seedPlatformErrorSpike(
	t *testing.T,
	store *Store,
	projectID int,
	status string,
	cycle int64,
	startsAt time.Time,
	bucket time.Time,
	count int,
) {
	t.Helper()
	ctx := context.Background()
	var endsAt any
	if status == platformAlertStatusResolved {
		endsAt = startsAt.Add(30 * time.Second)
	}
	if _, err := store.db.ExecContext(
		ctx,
		bind(
			store.dialect,
			`INSERT INTO platform_alert_states(project_id, kind, status, active_cycle, starts_at, ends_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		),
		projectID, platformAlertKindErrorRateSpike, status, cycle, startsAt, endsAt, startsAt,
	); err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		if _, err := store.db.ExecContext(
			ctx,
			bind(
				store.dialect,
				`INSERT INTO platform_error_rate_buckets(project_id, bucket_epoch, count) VALUES (?, ?, ?)`,
			),
			projectID,
			alignPlatformErrorBucket(bucket),
			count,
		); err != nil {
			t.Fatal(err)
		}
	}
}

// A recovery check must re-read the candidate's state under the row lock
// before it sums the window (issue #392): a candidate whose row resolved,
// moved to another cycle or vanished after it was listed is left alone, a
// still-spiking window keeps it firing, and only a count at or below the
// limit resolves the listed cycle.
func TestRecoverPlatformErrorSpikeRereadsLockedState(t *testing.T) {
	window := 5 * time.Minute
	now := time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)
	startsAt := now.Add(-2 * time.Minute)
	cases := []struct {
		name        string
		seedStatus  string // empty: no state row
		seedCycle   int64
		count       int
		wantResolve bool
	}{
		{
			name:        "count at limit resolves",
			seedStatus:  platformAlertStatusFiring,
			seedCycle:   1,
			count:       5,
			wantResolve: true,
		},
		{name: "still spiking", seedStatus: platformAlertStatusFiring, seedCycle: 1, count: 6},
		{name: "resolved meanwhile", seedStatus: platformAlertStatusResolved, seedCycle: 1, count: 0},
		{name: "next cycle meanwhile", seedStatus: platformAlertStatusFiring, seedCycle: 2, count: 0},
		{name: "row gone meanwhile", count: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, store *Store) {
				ctx := context.Background()
				project, err := store.CreateProject(ctx, "Recovery Reread", "recovery-reread")
				if err != nil {
					t.Fatal(err)
				}
				if tc.seedStatus != "" {
					seedPlatformErrorSpike(
						t,
						store,
						project.ID,
						tc.seedStatus,
						tc.seedCycle,
						startsAt,
						now.Add(-time.Minute),
						tc.count,
					)
				}
				before := platformSpikeStateRow(t, store, project.ID)
				// The candidate as listed before the state moved: firing cycle 1.
				candidate := platformErrorSpikeCandidate{
					projectID:   project.ID,
					projectSlug: project.Slug,
					cycle:       1,
					startsAt:    startsAt,
					threshold:   1, // per minute: limit is 5 over the 5-minute window
				}
				got, resolved, err := store.recoverPlatformErrorSpike(
					ctx,
					candidate,
					alignPlatformErrorBucket(now.Add(-window)),
					now,
					window,
				)
				if err != nil {
					t.Fatal(err)
				}
				after := platformSpikeStateRow(t, store, project.ID)
				if !tc.wantResolve {
					if resolved || !reflect.DeepEqual(after, before) {
						t.Fatalf("resolved=%v recovery=%#v state=%#v want untouched %#v", resolved, got, after, before)
					}
					return
				}
				want := PlatformErrorSpikeRecovery{
					ProjectID:   project.ID,
					ProjectSlug: project.Slug,
					Cycle:       1,
					StartsAt:    startsAt,
					EndsAt:      now,
					Count:       tc.count,
					Threshold:   1,
				}
				if !resolved || !reflect.DeepEqual(got, want) {
					t.Fatalf("resolved=%v recovery=%#v want %#v", resolved, got, want)
				}
				if after == nil || after.Status != platformAlertStatusResolved || after.Cycle != 1 ||
					after.EndsAt == nil || !after.EndsAt.Equal(now) {
					t.Fatalf("state=%#v want resolved cycle 1 ending at %v", after, now)
				}
			})
		})
	}
}

// A recovery check racing an uncommitted writer that holds the firing state
// row must wait for that writer and sum the window it committed (issue #392):
// a writer that pushed the window back over the limit keeps the spike firing,
// while one that rolled back leaves the stale count, which resolves it.
// PostgreSQL only: SQLite serializes the whole write transaction.
func TestCheckPlatformErrorRateRecoveriesWaitsForStateLock(t *testing.T) {
	window := 5 * time.Minute
	now := time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)
	startsAt := now.Add(-2 * time.Minute)
	cases := []struct {
		name        string
		commit      bool
		wantResolve bool
	}{
		{name: "writer commits spike", commit: true},
		{name: "writer rolls back", commit: false, wantResolve: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, store *Store) {
				if store.dialect != dialect.Postgres {
					t.Skip("only PostgreSQL runs concurrent metadata write transactions")
				}
				ctx := context.Background()
				project, err := store.CreateProject(ctx, "Recovery Lock", "recovery-lock")
				if err != nil {
					t.Fatal(err)
				}
				// Committed: firing cycle 1 with 3 occurrences, below the limit of 5.
				seedPlatformErrorSpike(
					t,
					store,
					project.ID,
					platformAlertStatusFiring,
					1,
					startsAt,
					now.Add(-time.Minute),
					3,
				)
				// threshold 1/min: the limit is 5 over the 5-minute window.
				if _, err := store.PutProjectAlertConfig(ctx, project.ID, ProjectAlertConfigUpdate{
					ErrorRateThresholdPerMinute: 1,
				}); err != nil {
					t.Fatal(err)
				}

				// The writer adds 3 more occurrences in a newer bucket and holds the
				// state row lock, as recordProjectErrorRate does before it commits.
				writer, err := store.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Rollback()
				if _, err := writer.ExecContext(
					ctx,
					`INSERT INTO platform_error_rate_buckets(project_id, bucket_epoch, count) VALUES ($1, $2, 3)`,
					project.ID, alignPlatformErrorBucket(now.Add(-30*time.Second)),
				); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.ExecContext(
					ctx,
					`SELECT status FROM platform_alert_states WHERE project_id = $1 AND kind = $2 FOR UPDATE`,
					project.ID, platformAlertKindErrorRateSpike,
				); err != nil {
					t.Fatal(err)
				}

				type outcome struct {
					recoveries []PlatformErrorSpikeRecovery
					err        error
				}
				done := make(chan outcome, 1)
				go func() {
					recoveries, err := store.CheckPlatformErrorRateRecoveries(ctx, now, window)
					done <- outcome{recoveries: recoveries, err: err}
				}()

				waitForBlockedQuery(
					t,
					store,
					done,
					`SELECT status, active_cycle, starts_at FROM platform_alert_states%FOR UPDATE`,
				)
				if tc.commit {
					err = writer.Commit()
				} else {
					err = writer.Rollback()
				}
				if err != nil {
					t.Fatal(err)
				}
				got := <-done
				if got.err != nil {
					t.Fatalf("recovery check failed: %v", got.err)
				}
				state := platformSpikeStateRow(t, store, project.ID)
				if !tc.wantResolve {
					if len(got.recoveries) != 0 {
						t.Fatalf("recoveries=%#v want none: the committed window is still spiking", got.recoveries)
					}
					if state == nil || state.Status != platformAlertStatusFiring || state.Cycle != 1 ||
						state.EndsAt != nil {
						t.Fatalf("state=%#v want still firing cycle 1", state)
					}
					return
				}
				if len(got.recoveries) != 1 || got.recoveries[0].Cycle != 1 || got.recoveries[0].Count != 3 {
					t.Fatalf("recoveries=%#v want cycle 1 resolved at count 3", got.recoveries)
				}
				if state == nil || state.Status != platformAlertStatusResolved || state.Cycle != 1 {
					t.Fatalf("state=%#v want resolved cycle 1", state)
				}
			})
		})
	}
}
