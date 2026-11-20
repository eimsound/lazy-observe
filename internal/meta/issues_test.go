package meta

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
)

func TestIssueAggregationLifecycleAndIsolation(t *testing.T) {
	forEachDialect(t, testIssueAggregationLifecycleAndIsolation)
}

func testIssueAggregationLifecycleAndIsolation(t *testing.T, store *Store) {
	ctx := context.Background()
	p1, _ := store.CreateProject(ctx, "One", "one")
	p2, _ := store.CreateProject(ctx, "Two", "two")
	now := time.Now().UTC().Truncate(time.Millisecond)
	delta := errortrack.IssueDelta{
		ProjectID:   p1.ID,
		Fingerprint: "same",
		Title:       "Type: value 1",
		Level:       "error",
		Count:       3,
		SessionBits: 3,
		FirstSeen:   now,
		LastSeen:    now,
		LastEventID: "e3",
		AcceptedAt:  now,
		AppliedAt:   now,
	}
	changes, err := store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{delta})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Kind != "new_issue" || changes[0].Title != delta.Title {
		t.Fatalf("new issue changes=%#v", changes)
	}
	rows, err := store.ListIssues(ctx, p1.ID, IssueListOptions{})
	if err != nil || len(rows) != 1 || rows[0].Count != 3 || rows[0].UserCount != 2 {
		t.Fatalf("initial rows=%#v err=%v", rows, err)
	}
	total, err := store.CountIssues(ctx, p1.ID, IssueListOptions{})
	if err != nil || total != 1 {
		t.Fatalf("initial total=%d err=%v", total, err)
	}
	issueID := rows[0].ID
	resolvedAt := now.Add(30 * time.Second)
	if _, err := store.SetIssueStatus(ctx, p1.ID, issueID, "resolved", resolvedAt); err != nil {
		t.Fatal(err)
	}
	delta.Count, delta.LastSeen, delta.LastEventID = 1, now.Add(time.Minute), "e4"
	delta.AcceptedAt, delta.AppliedAt = now.Add(time.Minute), now.Add(time.Minute+time.Second)
	changes, err = store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{delta})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Kind != "regression" || changes[0].IssueID != issueID {
		t.Fatalf("regression changes=%#v", changes)
	}
	resolved, _ := store.GetIssue(ctx, p1.ID, issueID)
	if resolved.Status != "open" || resolved.Count != 4 || resolved.RegressedAt == nil {
		t.Fatalf("regressed issue=%#v", resolved)
	}
	ignoredAt := now.Add(90 * time.Second)
	if _, err := store.SetIssueStatus(ctx, p1.ID, issueID, "ignored", ignoredAt); err != nil {
		t.Fatal(err)
	}
	delta.LastSeen, delta.LastEventID = now.Add(2*time.Minute), "e5"
	delta.AcceptedAt, delta.AppliedAt = now.Add(2*time.Minute), now.Add(2*time.Minute+time.Second)
	changes, err = store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{delta})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("ignored issue changes=%#v", changes)
	}
	ignored, _ := store.GetIssue(ctx, p1.ID, issueID)
	if ignored.Status != "ignored" || ignored.Count != 5 {
		t.Fatalf("ignored issue=%#v", ignored)
	}
	other, err := store.ListIssues(ctx, p2.ID, IssueListOptions{})
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-project rows=%#v err=%v", other, err)
	}
	otherTotal, err := store.CountIssues(ctx, p2.ID, IssueListOptions{})
	if err != nil || otherTotal != 0 {
		t.Fatalf("cross-project total=%d err=%v", otherTotal, err)
	}

	concurrent, err := store.CreateProject(ctx, "Concurrent", "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	const writers = 20
	writersStores := []*Store{store}
	if store.dialect == "postgres" {
		second, openErr := OpenStore(ctx, store.dbURL)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer second.Close()
		writersStores = append(writersStores, second)
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			writer := writersStores[i%len(writersStores)]
			_, err := writer.ApplyIssueDeltas(
				ctx,
				[]errortrack.IssueDelta{
					{
						ProjectID:   concurrent.ID,
						Fingerprint: "read-committed",
						Title:       "Concurrent",
						Level:       "error",
						Count:       1,
						SessionBits: uint64(1) << (i % 10),
						FirstSeen:   now,
						LastSeen:    now.Add(time.Duration(i) * time.Microsecond),
						LastEventID: fmt.Sprint(i),
						AcceptedAt:  now.Add(time.Duration(i) * time.Microsecond),
						AppliedAt:   now.Add(time.Second + time.Duration(i)*time.Microsecond),
					},
				},
			)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	concurrentRows, err := store.ListIssues(ctx, concurrent.ID, IssueListOptions{})
	if err != nil || len(concurrentRows) != 1 || concurrentRows[0].Count != writers {
		t.Fatalf("concurrent issue rows=%#v err=%v", concurrentRows, err)
	}
}

func TestIssueRegressionUsesAcceptedAtCutoff(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Acceptance Cutoff", "acceptance-cutoff")
		if err != nil {
			t.Fatal(err)
		}
		base := time.Now().UTC().Truncate(time.Millisecond)
		initial := errortrack.IssueDelta{
			ProjectID: project.ID, Fingerprint: "cutoff", Title: "CutoffError: boom", Level: "error",
			Count: 1, FirstSeen: base, LastSeen: base, LastEventID: "cutoff-initial",
			AcceptedAt: base.Add(time.Second), AppliedAt: base.Add(2 * time.Second),
		}
		changes, err := store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{initial})
		if err != nil || len(changes) != 1 {
			t.Fatalf("initial changes=%#v err=%v", changes, err)
		}
		issueID := changes[0].IssueID
		created, err := store.GetIssue(ctx, project.ID, issueID)
		if err != nil || !created.StatusChangedAt.Equal(initial.AppliedAt) {
			t.Fatalf("created issue=%#v err=%v", created, err)
		}

		resolvedAt := base.Add(10 * time.Second)
		if _, err := store.SetIssueStatus(ctx, project.ID, issueID, "resolved", resolvedAt); err != nil {
			t.Fatal(err)
		}
		backlog := initial
		backlog.Count = 1
		backlog.FirstSeen = base.Add(-time.Second)
		backlog.LastSeen = base.Add(-time.Second)
		backlog.LastEventID = "accepted-before-resolve"
		backlog.AcceptedAt = resolvedAt.Add(-time.Millisecond)
		backlog.AppliedAt = resolvedAt.Add(time.Minute)
		changes, err = store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{backlog})
		if err != nil || len(changes) != 0 {
			t.Fatalf("older accepted backlog changes=%#v err=%v", changes, err)
		}
		stillResolved, err := store.GetIssue(ctx, project.ID, issueID)
		if err != nil || stillResolved.Status != "resolved" || stillResolved.Count != 2 ||
			!stillResolved.StatusChangedAt.Equal(resolvedAt) {
			t.Fatalf("older accepted backlog issue=%#v err=%v", stillResolved, err)
		}

		firstRegression := initial
		firstRegression.Count = 1
		firstRegression.FirstSeen = base.Add(11 * time.Second)
		firstRegression.LastSeen = base.Add(11 * time.Second)
		firstRegression.LastEventID = "accepted-after-resolve-1"
		firstRegression.AcceptedAt = resolvedAt.Add(time.Millisecond)
		firstRegression.AppliedAt = resolvedAt.Add(2 * time.Minute)
		secondRegression := firstRegression
		secondRegression.LastSeen = base.Add(12 * time.Second)
		secondRegression.LastEventID = "accepted-after-resolve-2"
		secondRegression.AcceptedAt = resolvedAt.Add(2 * time.Millisecond)
		secondRegression.AppliedAt = resolvedAt.Add(3 * time.Minute)
		changes, err = store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{firstRegression, secondRegression})
		if err != nil || len(changes) != 1 || changes[0].Kind != "regression" {
			t.Fatalf("later accepted regressions=%#v err=%v", changes, err)
		}
		regressed, err := store.GetIssue(ctx, project.ID, issueID)
		if err != nil || regressed.Status != "open" || regressed.Count != 4 ||
			!regressed.StatusChangedAt.Equal(firstRegression.AppliedAt) {
			t.Fatalf("regressed issue=%#v err=%v", regressed, err)
		}

		ignoredAt := base.Add(4 * time.Minute)
		if _, err := store.SetIssueStatus(ctx, project.ID, issueID, "ignored", ignoredAt); err != nil {
			t.Fatal(err)
		}
		ignoredEvent := secondRegression
		ignoredEvent.LastEventID = "accepted-after-ignore"
		ignoredEvent.AcceptedAt = ignoredAt.Add(time.Millisecond)
		ignoredEvent.AppliedAt = ignoredAt.Add(time.Minute)
		changes, err = store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{ignoredEvent})
		ignored, getErr := store.GetIssue(ctx, project.ID, issueID)
		if err != nil || getErr != nil || len(changes) != 0 || ignored.Status != "ignored" || ignored.Count != 5 ||
			!ignored.StatusChangedAt.Equal(ignoredAt) {
			t.Fatalf("ignored event changes=%#v issue=%#v err=%v get_err=%v", changes, ignored, err, getErr)
		}
	})
}

func TestSetIssuesStatusHandlesLeadingZeroAndListIssuesHandlesOverflowPage(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Edge", "edge")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		changes, err := store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{
			{
				ProjectID: project.ID, Fingerprint: "fp-1", Title: "One", Level: "error",
				Count: 1, FirstSeen: now, LastSeen: now, LastEventID: "e1", AcceptedAt: now, AppliedAt: now,
			},
			{
				ProjectID: project.ID, Fingerprint: "fp-2", Title: "Two", Level: "error",
				Count: 1, FirstSeen: now, LastSeen: now, LastEventID: "e2", AcceptedAt: now, AppliedAt: now,
			},
		})
		if err != nil || len(changes) != 2 {
			t.Fatalf("changes=%#v err=%v", changes, err)
		}
		updated, err := store.SetIssuesStatus(
			ctx,
			project.ID,
			[]int{0, changes[0].IssueID, changes[1].IssueID},
			"resolved",
			now.Add(time.Second),
		)
		if err != nil || updated != 2 {
			t.Fatalf("SetIssuesStatus with leading zero updated=%d want 2 err=%v", updated, err)
		}
		rows, err := store.ListIssues(ctx, project.ID, IssueListOptions{Page: math.MaxInt})
		if err != nil || len(rows) != 0 {
			t.Fatalf("ListIssues with MaxInt page rows=%#v err=%v", rows, err)
		}
	})
}
