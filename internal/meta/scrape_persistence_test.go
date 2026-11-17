package meta

import (
	"context"
	"testing"
)

func TestScrapeTargetPersistenceAndConfigSnapshotAreProjectScoped(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		for _, project := range []struct{ name, slug string }{{"Alpha", "alpha"}, {"Beta", "beta"}} {
			if _, err := store.CreateProject(ctx, project.name, project.slug); err != nil {
				t.Fatal(err)
			}
		}
		alpha, err := store.CreateScrapeTarget(ctx, ScrapeTargetCreate{
			ProjectSlug: "alpha", Name: "node", URL: "http://127.0.0.1:9100/metrics", Interval: "15s",
		})
		if err != nil {
			t.Fatal(err)
		}
		disabled := false
		if _, err := store.CreateScrapeTarget(ctx, ScrapeTargetCreate{
			ProjectSlug: "beta", Name: "off", URL: "http://127.0.0.1:9200/metrics", Interval: "30s", Enabled: &disabled,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ScrapeTarget(ctx, "beta", alpha.ID); !IsNotFound(err) {
			t.Fatalf("cross-project get error=%v, want not found", err)
		}
		name := "hijacked"
		if _, err := store.UpdateScrapeTarget(
			ctx,
			"beta",
			alpha.ID,
			ScrapeTargetUpdate{Name: &name},
		); !IsNotFound(
			err,
		) {
			t.Fatalf("cross-project update error=%v, want not found", err)
		}
		if err := store.DeleteScrapeTarget(ctx, "beta", alpha.ID); !IsNotFound(err) {
			t.Fatalf("cross-project delete error=%v, want not found", err)
		}
		preserved, err := store.ScrapeTarget(ctx, "alpha", alpha.ID)
		if err != nil || preserved.Name != "node" {
			t.Fatalf("alpha target after cross-project writes=%+v err=%v", preserved, err)
		}
		alphaTargets, err := store.ListScrapeTargets(ctx, "alpha")
		if err != nil || len(alphaTargets) != 1 || alphaTargets[0].Project.Slug != "alpha" {
			t.Fatalf("alpha targets=%+v err=%v", alphaTargets, err)
		}
		snapshot, err := store.ScrapeConfigSnapshot(ctx)
		if err != nil || len(snapshot) != 1 || snapshot[0].ID != alpha.ID {
			t.Fatalf("snapshot=%+v err=%v", snapshot, err)
		}
	})
}
