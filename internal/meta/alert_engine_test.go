package meta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestAlertEngineMembershipLeaseAndFencing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.client.Project.Create().SetName("Ownership").SetSlug("ownership").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.client.AlertRule.Create().
			SetProjectID(project.ID).
			SetName("Owned rule").
			SetExpr("up == 0").
			Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcquireAlertRuleLease(
			ctx,
			rule.ID,
			"engine-a",
			time.Minute,
		); !errors.Is(
			err,
			ErrInactiveAlertEngineMember,
		) {
			t.Fatalf("inactive member acquire error=%v", err)
		}
		for _, memberID := range []string{"engine-a", "engine-b"} {
			member, err := store.HeartbeatAlertEngine(ctx, memberID, time.Minute)
			if err != nil || member.MemberID != memberID || !member.ExpiresAt.After(member.HeartbeatAt) {
				t.Fatalf("heartbeat member=%#v err=%v", member, err)
			}
		}
		members, err := store.ActiveAlertEngineMembers(ctx)
		if err != nil || len(members) != 2 || members[0].MemberID != "engine-a" || members[1].MemberID != "engine-b" {
			t.Fatalf("active members=%v err=%v", members, err)
		}

		leaseA, err := store.AcquireAlertRuleLease(ctx, rule.ID, "engine-a", time.Minute)
		if err != nil || leaseA.Epoch != 1 || leaseA.OwnerID != "engine-a" {
			t.Fatalf("lease A=%#v err=%v", leaseA, err)
		}
		renewedA, err := store.AcquireAlertRuleLease(ctx, rule.ID, "engine-a", time.Minute)
		if err != nil || renewedA.Epoch != leaseA.Epoch {
			t.Fatalf("same-owner acquire=%#v err=%v", renewedA, err)
		}
		if _, err := store.AcquireAlertRuleLease(
			ctx,
			rule.ID,
			"engine-b",
			time.Minute,
		); !errors.Is(
			err,
			ErrRuleLeaseHeld,
		) {
			t.Fatalf("competing acquire error=%v", err)
		}

		now := time.Now().UTC()
		pending := now.Add(-time.Minute)
		value := 1.5
		state := AlertRuleState{
			RuleID:          rule.ID,
			Fingerprint:     "instance=api-1",
			Status:          "pending",
			PendingSince:    &pending,
			LastEvaluatedAt: now,
			Labels:          map[string]string{"instance": "api-1"},
			Annotations:     map[string]string{"summary": "down"},
			Value:           &value,
		}
		if err := store.PutAlertRuleState(ctx, state, "engine-a", leaseA.Epoch); err != nil {
			t.Fatal(err)
		}
		expireLease := `UPDATE alert_rule_leases SET expires_at = datetime('now', '-1 hour') WHERE rule_id = ?`
		if store.dialect == "postgres" {
			expireLease = `UPDATE alert_rule_leases SET expires_at = CURRENT_TIMESTAMP - INTERVAL '1 hour' WHERE rule_id = ?`
		}
		if _, err := store.db.ExecContext(ctx, bind(store.dialect, expireLease), rule.ID); err != nil {
			t.Fatal(err)
		}
		leaseB, err := store.AcquireAlertRuleLease(ctx, rule.ID, "engine-b", time.Minute)
		if err != nil || leaseB.Epoch != leaseA.Epoch+1 {
			t.Fatalf("lease B=%#v err=%v", leaseB, err)
		}
		if err := store.PutAlertRuleState(ctx, state, "engine-a", leaseA.Epoch); !errors.Is(err, ErrStaleRuleFence) {
			t.Fatalf("stale owner state error=%v", err)
		}
		firing := now
		state.Status = "firing"
		state.FiringSince = &firing
		state.LastEvaluatedAt = now.Add(time.Second)
		if err := store.PutAlertRuleState(ctx, state, "engine-b", leaseB.Epoch); err != nil {
			t.Fatal(err)
		}
		states, err := store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 1 || states[0].Status != "firing" || states[0].LeaseEpoch != leaseB.Epoch ||
			states[0].Labels["instance"] != "api-1" {
			t.Fatalf("states=%#v err=%v", states, err)
		}
		if _, err := store.RenewAlertRuleLease(
			ctx,
			rule.ID,
			"engine-a",
			leaseA.Epoch,
			time.Minute,
		); !errors.Is(
			err,
			ErrStaleRuleFence,
		) {
			t.Fatalf("stale renew error=%v", err)
		}
		if _, err := store.RenewAlertRuleLease(ctx, rule.ID, "engine-b", leaseB.Epoch, time.Minute); err != nil {
			t.Fatal(err)
		}
		if released, err := store.ReleaseAlertRuleLease(
			ctx,
			rule.ID,
			"engine-a",
			leaseA.Epoch,
		); err != nil ||
			released {
			t.Fatalf("stale release=%v err=%v", released, err)
		}
		if released, err := store.ReleaseAlertRuleLease(
			ctx,
			rule.ID,
			"engine-b",
			leaseB.Epoch,
		); err != nil ||
			!released {
			t.Fatalf("current release=%v err=%v", released, err)
		}
		if err := store.PutAlertRuleState(ctx, state, "engine-b", leaseB.Epoch); !errors.Is(err, ErrStaleRuleFence) {
			t.Fatalf("released lease write error=%v", err)
		}

		if _, err := store.db.ExecContext(
			ctx,
			`UPDATE alert_engine_members SET expires_at = CURRENT_TIMESTAMP`,
		); err != nil {
			t.Fatal(err)
		}
		if deleted, err := store.DeleteExpiredAlertEngineMembers(ctx); err != nil || deleted != 2 {
			t.Fatalf("deleted members=%d err=%v", deleted, err)
		}
	})
}

// A lease acquired mid-second must keep its full TTL. SQLite datetime() and CURRENT_TIMESTAMP
// have whole-second precision, so truncated expiries made a 1s lease die at the next second boundary.
func TestAlertRuleLeaseKeepsFullTTLAcrossSecondBoundary(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.client.Project.Create().SetName("Precision").SetSlug("precision").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.client.AlertRule.Create().SetProjectID(project.ID).SetName("Precise").SetExpr("up").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		const ttl = time.Second
		sleepUntilSubsecond(600 * time.Millisecond)
		before := time.Now()
		member, err := store.HeartbeatAlertEngine(ctx, "engine-precise", ttl)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireAlertRuleLease(ctx, rule.ID, "engine-precise", ttl)
		if err != nil {
			t.Fatal(err)
		}
		minExpiry := before.Add(ttl - 50*time.Millisecond)
		if member.ExpiresAt.Before(minExpiry) || lease.ExpiresAt.Before(minExpiry) {
			t.Fatalf("expiries truncated: acquired=%s member=%s lease=%s", before.UTC().Format(time.StampMilli),
				member.ExpiresAt.UTC().Format(time.StampMilli), lease.ExpiresAt.UTC().Format(time.StampMilli))
		}
		sleepUntilSubsecond(50 * time.Millisecond)
		if _, err := store.RenewAlertRuleLease(ctx, rule.ID, "engine-precise", lease.Epoch, ttl); err != nil {
			t.Fatalf("renew %s after acquire within TTL: %v", time.Since(before).Round(time.Millisecond), err)
		}
	})
}

// sleepUntilSubsecond sleeps until the wall clock next reaches the given offset within a second.
func sleepUntilSubsecond(offset time.Duration) {
	now := time.Now()
	target := now.Truncate(time.Second).Add(offset)
	if !target.After(now) {
		target = target.Add(time.Second)
	}
	time.Sleep(target.Sub(now))
}

func TestDeleteAlertEngineMember(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		if _, err := store.HeartbeatAlertEngine(ctx, "engine-graceful", time.Minute); err != nil {
			t.Fatal(err)
		}
		if deleted, err := store.DeleteAlertEngineMember(ctx, "engine-graceful"); err != nil || !deleted {
			t.Fatalf("delete member=%v err=%v", deleted, err)
		}
		if deleted, err := store.DeleteAlertEngineMember(ctx, "engine-graceful"); err != nil || deleted {
			t.Fatalf("delete missing member=%v err=%v", deleted, err)
		}
	})
}

func TestPostgresConcurrentAlertRuleLeaseHasOneWinner(t *testing.T) {
	if os.Getenv("LO_TEST_PG") != "1" {
		t.Skip("set LO_TEST_PG=1 to run PostgreSQL metadata tests")
	}
	store := newPostgresTestStore(t)
	ctx := context.Background()
	project, err := store.client.Project.Create().SetName("Lease race").SetSlug("lease-race").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.client.AlertRule.Create().SetProjectID(project.ID).SetName("Race").SetExpr("up").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, memberID := range []string{"engine-a", "engine-b"} {
		if _, err := store.HeartbeatAlertEngine(ctx, memberID, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, memberID := range []string{"engine-a", "engine-b"} {
		wg.Add(1)
		go func(memberID string) {
			defer wg.Done()
			<-start
			_, err := store.AcquireAlertRuleLease(ctx, rule.ID, memberID, time.Minute)
			results <- err
		}(memberID)
	}
	close(start)
	wg.Wait()
	close(results)
	winners, held := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrRuleLeaseHeld):
			held++
		default:
			t.Fatal(fmt.Errorf("unexpected lease race result: %w", err))
		}
	}
	if winners != 1 || held != 1 {
		t.Fatalf("lease race winners=%d held=%d", winners, held)
	}
}
