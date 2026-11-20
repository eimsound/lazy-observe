package meta

import (
	"context"
	"sync"
	"sync/atomic"
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

func TestAISettingsCompareAndSwapConcurrency(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		base := AISettings{
			Enabled:          true,
			Provider:         "openai",
			Model:            "gpt-4o",
			APIKeyCiphertext: "cipher-0",
			MaxTokens:        1024,
			UpdatedAt:        time.Now().UTC().Truncate(time.Microsecond),
			UpdatedBy:        "admin-0",
		}
		// Initial insert CAS with expected=nil succeeds once, then conflicts.
		ok, err := store.CompareAndSwapAISettings(ctx, nil, base)
		if err != nil || !ok {
			t.Fatalf("initial CAS ok=%v err=%v", ok, err)
		}
		ok, err = store.CompareAndSwapAISettings(ctx, nil, base)
		if err != nil || ok {
			t.Fatalf("duplicate initial CAS ok=%v err=%v, want false/nil", ok, err)
		}

		current, exists, err := store.GetAISettings(ctx)
		if err != nil || !exists {
			t.Fatalf("GetAISettings exists=%v err=%v", exists, err)
		}

		// Concurrent CAS against the same snapshot: exactly one writer wins.
		const workers = 8
		var wins atomic.Int32
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for i := range workers {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				expected := current
				next := current
				next.MaxTokens = 2000 + idx
				next.UpdatedBy = "admin"
				swapped, casErr := store.CompareAndSwapAISettings(ctx, &expected, next)
				if casErr != nil {
					errs <- casErr
					return
				}
				if swapped {
					wins.Add(1)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for casErr := range errs {
			t.Fatalf("concurrent CAS error: %v", casErr)
		}
		if wins.Load() != 1 {
			t.Fatalf("concurrent CAS winners = %d, want 1", wins.Load())
		}
	})
}
