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
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case out := <-done:
			t.Fatalf("record returned before blocking on the state insert: %#v", out)
		default:
		}
		var blocked int
		if err := store.db.QueryRowContext(
			context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND query LIKE 'INSERT INTO platform_alert_states%'`,
		).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("record never blocked on the platform_alert_states insert")
}
