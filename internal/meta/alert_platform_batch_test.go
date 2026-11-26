package meta

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type platformBucketRow struct {
	Epoch int64
	Count int
}

type platformStateRow struct {
	Status   string
	Cycle    int64
	StartsAt time.Time
	EndsAt   *time.Time
}

func platformBucketRows(t *testing.T, store *Store, projectID int) []platformBucketRow {
	t.Helper()
	rows, err := store.db.QueryContext(
		context.Background(),
		bind(
			store.dialect,
			`SELECT bucket_epoch, count FROM platform_error_rate_buckets WHERE project_id = ? ORDER BY bucket_epoch`,
		),
		projectID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []platformBucketRow
	for rows.Next() {
		var row platformBucketRow
		if err := rows.Scan(&row.Epoch, &row.Count); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func platformSpikeStateRow(t *testing.T, store *Store, projectID int) *platformStateRow {
	t.Helper()
	var (
		row      platformStateRow
		startsAt sql.NullTime
		endsAt   sql.NullTime
	)
	err := store.db.QueryRowContext(
		context.Background(),
		bind(
			store.dialect,
			`SELECT status, active_cycle, starts_at, ends_at FROM platform_alert_states WHERE project_id = ? AND kind = ?`,
		),
		projectID,
		platformAlertKindErrorRateSpike,
	).Scan(&row.Status, &row.Cycle, &startsAt, &endsAt)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if startsAt.Valid {
		row.StartsAt = startsAt.Time.UTC()
	}
	if endsAt.Valid {
		ts := endsAt.Time.UTC()
		row.EndsAt = &ts
	}
	return &row
}

// TestRecordPlatformErrorRatesBatchMatchesSequentialRecords pins issue #375:
// one batched call must leave the same buckets, durable state and transition
// sequence as recording the same occurrences one by one, while opening a
// single write transaction for every project in the batch.
func TestRecordPlatformErrorRatesBatchMatchesSequentialRecords(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		window := 5 * time.Minute
		t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		spaced := func(start time.Time, n int, step time.Duration) []time.Time {
			out := make([]time.Time, n)
			for i := range out {
				out[i] = start.Add(time.Duration(i) * step)
			}
			return out
		}
		type scenario struct {
			name      string
			threshold int
			seed      func(t *testing.T, projectID int)
			now       time.Time
			events    []time.Time
			want      []PlatformErrorSpikeTransition
		}
		seedSpike := func(t *testing.T, projectID int) {
			t.Helper()
			for _, ts := range spaced(t0, 6, time.Second) {
				if _, err := store.RecordPlatformErrorRate(ctx, projectID, ts, ts, window, 1); err != nil {
					t.Fatal(err)
				}
			}
		}
		late := t0.Add(6 * time.Minute)
		scenarios := []scenario{
			{
				name:      "fresh project fires once across buckets",
				threshold: 1,
				now:       t0.Add(time.Minute),
				events:    spaced(t0, 8, 7*time.Second),
				want:      []PlatformErrorSpikeTransition{{Transition: "firing", Cycle: 1, Count: 6}},
			},
			{
				// The window slid past the previous spike but the recovery loop has
				// not run yet: the first event resolves cycle 1, the sixth re-fires.
				name:      "stale firing resolves then re-fires next cycle",
				threshold: 1,
				seed:      seedSpike,
				now:       late,
				events:    spaced(late.Add(-20*time.Second), 7, time.Second),
				want: []PlatformErrorSpikeTransition{
					{Transition: "resolved", Cycle: 1, Count: 1},
					{Transition: "firing", Cycle: 2, Count: 6},
				},
			},
			{
				name:      "resolved project advances cycle",
				threshold: 1,
				seed: func(t *testing.T, projectID int) {
					seedSpike(t, projectID)
					if _, err := store.ClearPlatformErrorRateState(ctx, projectID, t0.Add(time.Minute)); err != nil {
						t.Fatal(err)
					}
				},
				now:    t0.Add(2 * time.Minute),
				events: spaced(t0.Add(time.Minute), 6, time.Second),
				want:   []PlatformErrorSpikeTransition{{Transition: "firing", Cycle: 2, Count: 6}},
			},
			{
				name:      "still firing stays silent",
				threshold: 1,
				seed:      seedSpike,
				now:       t0.Add(time.Minute),
				events:    spaced(t0.Add(30*time.Second), 3, time.Second),
			},
			{
				name:      "threshold zero resolves firing on first event",
				threshold: 0,
				seed:      seedSpike,
				now:       t0.Add(time.Minute),
				events:    spaced(t0.Add(30*time.Second), 2, time.Second),
				want:      []PlatformErrorSpikeTransition{{Transition: "resolved", Cycle: 1, Count: 7}},
			},
			{
				name:      "expired and zero occurrences are ignored",
				threshold: 1,
				now:       t0.Add(time.Hour),
				events:    []time.Time{{}, t0, t0.Add(time.Second)},
			},
		}

		type pair struct{ seq, batch int }
		projects := make([]pair, len(scenarios))
		for i, sc := range scenarios {
			for j, role := range []string{"seq", "batch"} {
				p, err := store.CreateProject(ctx, fmt.Sprintf("%s %d", role, i), fmt.Sprintf("%s-%d", role, i))
				if err != nil {
					t.Fatal(err)
				}
				if sc.seed != nil {
					sc.seed(t, p.ID)
				}
				if j == 0 {
					projects[i].seq = p.ID
				} else {
					projects[i].batch = p.ID
				}
			}
		}

		// Sequential reference: one RecordPlatformErrorRate (and one
		// transaction) per in-window occurrence, at the batch's fixed now.
		seqTransitions := make([][]PlatformErrorSpikeTransition, len(scenarios))
		seqTxBefore := store.platformErrorRateTxs.Load()
		inWindow := 0
		for i, sc := range scenarios {
			for _, ts := range sc.events {
				if !ts.IsZero() && !ts.Before(sc.now.Add(-window)) {
					inWindow++
				}
				tr, err := store.RecordPlatformErrorRate(ctx, projects[i].seq, ts, sc.now, window, sc.threshold)
				if err != nil {
					t.Fatal(err)
				}
				if tr.Transition != "" {
					seqTransitions[i] = append(seqTransitions[i], tr)
				}
			}
		}
		seqTxs := store.platformErrorRateTxs.Load() - seqTxBefore
		if seqTxs != uint64(inWindow) {
			t.Fatalf("sequential reference opened %d write transactions, want one per in-window event (%d)",
				seqTxs, inWindow)
		}
		var batchTxs uint64

		// Batched: every scenario sharing one now is applied in one call.
		batchTransitions := make([][]PlatformErrorSpikeTransition, len(scenarios))
		byNow := map[time.Time][]int{}
		var nows []time.Time
		for i, sc := range scenarios {
			if _, ok := byNow[sc.now]; !ok {
				nows = append(nows, sc.now)
			}
			byNow[sc.now] = append(byNow[sc.now], i)
		}
		for _, now := range nows {
			idx := byNow[now]
			batches := make([]PlatformErrorRateBatch, 0, len(idx))
			for k := len(idx) - 1; k >= 0; k-- { // reverse input order: results must still sort by project
				sc := scenarios[idx[k]]
				batches = append(batches, PlatformErrorRateBatch{
					ProjectID: projects[idx[k]].batch, ThresholdPerMin: sc.threshold, OccurredAt: sc.events,
				})
			}
			before := store.platformErrorRateTxs.Load()
			results, err := store.RecordPlatformErrorRates(ctx, now, window, batches)
			if err != nil {
				t.Fatal(err)
			}
			got := store.platformErrorRateTxs.Load() - before
			batchTxs += got
			if got > 1 {
				t.Fatalf("now=%s batched record opened %d write transactions, want at most 1", now, got)
			}
			for r := 1; r < len(results); r++ {
				if results[r-1].ProjectID >= results[r].ProjectID {
					t.Fatalf("results not ordered by project_id: %#v", results)
				}
			}
			for _, res := range results {
				for _, i := range idx {
					if projects[i].batch == res.ProjectID {
						batchTransitions[i] = res.Transitions
					}
				}
			}
		}

		t.Logf("write transactions: sequential=%d (one per in-window event) batched=%d (%d calls, at most one each)",
			seqTxs, batchTxs,
			len(nows))
		for i, sc := range scenarios {
			t.Run(sc.name, func(t *testing.T) {
				if !reflect.DeepEqual(batchTransitions[i], seqTransitions[i]) {
					t.Fatalf("batch transitions=%#v\nsequential=%#v", batchTransitions[i], seqTransitions[i])
				}
				if len(batchTransitions[i]) != len(sc.want) {
					t.Fatalf("transitions=%#v want %#v", batchTransitions[i], sc.want)
				}
				for k, want := range sc.want {
					got := batchTransitions[i][k]
					if got.Transition != want.Transition || got.Cycle != want.Cycle || got.Count != want.Count {
						t.Fatalf("transition %d=%#v want %#v", k, got, want)
					}
					if want.Transition == "firing" && !got.StartsAt.Equal(sc.now) {
						t.Fatalf("firing starts_at=%s want %s", got.StartsAt, sc.now)
					}
					if want.Transition == "resolved" && !got.EndsAt.Equal(sc.now) {
						t.Fatalf("resolved ends_at=%s want %s", got.EndsAt, sc.now)
					}
				}
				if seq, batch := platformBucketRows(t, store, projects[i].seq),
					platformBucketRows(t, store, projects[i].batch); !reflect.DeepEqual(seq, batch) {
					t.Fatalf("buckets batch=%#v sequential=%#v", batch, seq)
				}
				seqState := platformSpikeStateRow(t, store, projects[i].seq)
				batchState := platformSpikeStateRow(t, store, projects[i].batch)
				if !reflect.DeepEqual(seqState, batchState) {
					t.Fatalf("state batch=%#v sequential=%#v", batchState, seqState)
				}
			})
		}
	})
}

func TestRecordPlatformErrorRatesAggregatesBucketsInOneTransaction(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		window := 5 * time.Minute
		now := time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)
		a, err := store.CreateProject(ctx, "Agg A", "agg-a")
		if err != nil {
			t.Fatal(err)
		}
		b, err := store.CreateProject(ctx, "Agg B", "agg-b")
		if err != nil {
			t.Fatal(err)
		}
		bucket := time.Date(2026, 8, 1, 12, 3, 0, 0, time.UTC)
		expired := now.Add(-window - 10*time.Second)
		// Pre-existing rows: one bucket that the batch must accumulate onto (the
		// ON CONFLICT path) and one expired bucket the batch must prune.
		for _, ts := range []time.Time{bucket, bucket.Add(time.Second)} {
			if _, err := store.RecordPlatformErrorRate(ctx, a.ID, ts, now, window, 100); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.RecordPlatformErrorRate(ctx, a.ID, expired, expired, window, 100); err != nil {
			t.Fatal(err)
		}
		if got := platformBucketRows(t, store, a.ID); len(got) != 2 {
			t.Fatalf("precondition buckets=%#v", got)
		}

		before := store.platformErrorRateTxs.Load()
		results, err := store.RecordPlatformErrorRates(ctx, now, window, []PlatformErrorRateBatch{
			{ProjectID: b.ID, ThresholdPerMin: 100, OccurredAt: []time.Time{bucket.Add(12 * time.Second)}},
			{ProjectID: a.ID, ThresholdPerMin: 100, OccurredAt: []time.Time{
				bucket.Add(2 * time.Second), bucket.Add(9 * time.Second), bucket.Add(3 * time.Second),
				bucket.Add(10 * time.Second), bucket.Add(19 * time.Second), expired,
			}},
			// A duplicate project entry is merged into the same accounting step.
			{ProjectID: a.ID, ThresholdPerMin: 100, OccurredAt: []time.Time{bucket.Add(4 * time.Second)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := store.platformErrorRateTxs.Load() - before; got != 1 {
			t.Fatalf("batched record opened %d write transactions, want 1", got)
		}
		if len(results) != 2 || results[0].ProjectID != a.ID || results[1].ProjectID != b.ID {
			t.Fatalf("results=%#v", results)
		}
		if results[0].Count != 8 || results[1].Count != 1 || len(results[0].Transitions) != 0 {
			t.Fatalf("results=%#v want counts a=8 b=1 and no transitions", results)
		}
		epoch := bucket.Unix()
		if got, want := platformBucketRows(t, store, a.ID), []platformBucketRow{
			{Epoch: epoch, Count: 6},
			{Epoch: epoch + 10, Count: 2},
		}; !reflect.DeepEqual(got, want) {
			t.Fatalf("project a buckets=%#v want %#v", got, want)
		}
		if got, want := platformBucketRows(t, store, b.ID), []platformBucketRow{
			{Epoch: epoch + 10, Count: 1},
		}; !reflect.DeepEqual(got, want) {
			t.Fatalf("project b buckets=%#v want %#v", got, want)
		}

		// Nothing in window: no transaction at all.
		before = store.platformErrorRateTxs.Load()
		results, err = store.RecordPlatformErrorRates(ctx, now, window, []PlatformErrorRateBatch{
			{ProjectID: a.ID, ThresholdPerMin: 1, OccurredAt: []time.Time{expired, {}}},
		})
		if err != nil || len(results) != 0 || store.platformErrorRateTxs.Load() != before {
			t.Fatalf(
				"out-of-window batch results=%#v err=%v txs=%d",
				results,
				err,
				store.platformErrorRateTxs.Load()-before,
			)
		}

		if _, err := store.RecordPlatformErrorRates(ctx, now, 0, nil); err == nil {
			t.Fatal("zero window accepted")
		}
		if _, err := store.RecordPlatformErrorRates(ctx, now, window, []PlatformErrorRateBatch{
			{ProjectID: 0, OccurredAt: []time.Time{now}},
		}); err == nil {
			t.Fatal("project 0 accepted")
		}
	})
}

// A project that fails mid-batch (here a missing project violating the
// projects foreign key, as when it is deleted while its events are in flight)
// is rolled back alone; the rest of the batch still commits in the same
// transaction and is returned next to the joined error.
func TestRecordPlatformErrorRatesIsolatesFailingProject(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		window := 5 * time.Minute
		now := time.Date(2026, 8, 1, 12, 5, 0, 0, time.UTC)
		low, err := store.CreateProject(ctx, "Iso Low", "iso-low")
		if err != nil {
			t.Fatal(err)
		}
		high, err := store.CreateProject(ctx, "Iso High", "iso-high")
		if err != nil {
			t.Fatal(err)
		}
		missing := high.ID + 1000
		events := []time.Time{now.Add(-3 * time.Second), now.Add(-2 * time.Second), now.Add(-time.Second)}
		before := store.platformErrorRateTxs.Load()
		results, err := store.RecordPlatformErrorRates(ctx, now, window, []PlatformErrorRateBatch{
			{ProjectID: low.ID, ThresholdPerMin: 1, OccurredAt: events},
			{ProjectID: missing, ThresholdPerMin: 1, OccurredAt: events},
			{ProjectID: high.ID, ThresholdPerMin: 1, OccurredAt: events},
		})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("project %d", missing)) {
			t.Fatalf("err=%v want failure naming project %d", err, missing)
		}
		if got := store.platformErrorRateTxs.Load() - before; got != 1 {
			t.Fatalf("batched record opened %d write transactions, want 1", got)
		}
		if len(results) != 2 || results[0].ProjectID != low.ID || results[1].ProjectID != high.ID ||
			results[0].Count != 3 || results[1].Count != 3 {
			t.Fatalf("results=%#v", results)
		}
		for _, projectID := range []int{low.ID, high.ID} {
			if got := platformBucketRows(t, store, projectID); len(got) != 1 || got[0].Count != 3 {
				t.Fatalf("project %d buckets=%#v want one committed bucket of 3", projectID, got)
			}
		}
		if got := platformBucketRows(t, store, missing); len(got) != 0 {
			t.Fatalf("missing project buckets=%#v", got)
		}
	})
}
