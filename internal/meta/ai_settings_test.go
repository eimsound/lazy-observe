package meta

import (
	"context"
	"testing"
	"time"
)

func TestAISettingsSingleRowUpsert(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()

		if _, ok, err := store.GetAISettings(ctx); err != nil {
			t.Fatalf("GetAISettings on fresh store: %v", err)
		} else if ok {
			t.Fatal("fresh store reports AI settings present")
		}

		first := AISettings{
			Enabled:          true,
			Provider:         "deepseek",
			BaseURL:          "https://api.deepseek.com",
			Model:            "deepseek-chat",
			APIKeyCiphertext: "sealed-key-1",
			MaxTokens:        2048,
			UpdatedAt:        time.Now().UTC().Truncate(time.Microsecond),
			UpdatedBy:        "admin",
		}
		if err := store.UpsertAISettings(ctx, first); err != nil {
			t.Fatalf("first UpsertAISettings: %v", err)
		}
		got, ok, err := store.GetAISettings(ctx)
		if err != nil {
			t.Fatalf("GetAISettings after upsert: %v", err)
		}
		if !ok || !equalAISettings(got, first) {
			t.Fatalf("stored settings = %#v ok=%v want %#v", got, ok, first)
		}

		second := AISettings{
			Enabled:          false,
			Provider:         "openai",
			Model:            "gpt-4o-mini",
			APIKeyCiphertext: "sealed-key-2",
			MaxTokens:        1024,
			UpdatedAt:        first.UpdatedAt.Add(time.Minute),
			UpdatedBy:        "admin",
		}
		if err := store.UpsertAISettings(ctx, second); err != nil {
			t.Fatalf("second UpsertAISettings: %v", err)
		}
		got, ok, err = store.GetAISettings(ctx)
		if err != nil {
			t.Fatalf("GetAISettings after second upsert: %v", err)
		}
		if !ok || !equalAISettings(got, second) {
			t.Fatalf("settings after second upsert = %#v ok=%v want %#v", got, ok, second)
		}

		count, err := store.client.AISetting.Query().Count(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("ai_settings row count = %d, want 1", count)
		}
	})
}

func equalAISettings(got, want AISettings) bool {
	gotAt, wantAt := got.UpdatedAt, want.UpdatedAt
	got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}
	return got == want && gotAt.Equal(wantAt)
}
