package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// blockingProjectStore parks the automatic Issue AI worker inside its first
// job so the queue can be filled deterministically.
type blockingProjectStore struct {
	fakeProjectStore
	entered chan struct{}
	release chan struct{}
}

func (s blockingProjectStore) GetProject(context.Context, int) (*meta.Project, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return nil, errors.New("metadata offline")
}

type unusedIssueStore struct{ IssueStore }

func TestAutomaticIssueAIReportsDroppedAndFailedWork(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	var logs bytes.Buffer
	store := blockingProjectStore{
		fakeProjectStore: fakeProjectStore{tokens: &fakeTokens{}},
		entered:          make(chan struct{}, 1),
		release:          make(chan struct{}),
	}
	gw := newTestGateway(Config{
		Tokens: &fakeTokens{}, Store: store, Issues: unusedIssueStore{},
		AIEnabled: true, AIClient: &issueAIFake{}, MeterProvider: provider,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	change := errortrack.IssueChange{Kind: "new_issue", ProjectID: 7, IssueID: 1}
	if err := gw.HandleIssueChange(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic Issue AI worker did not start the first job")
	}
	for range issueAIQueueSize + 1 { // fill the queue, then overflow it once
		_ = gw.HandleIssueChange(context.Background(), change)
	}
	if got := issueAIOutcomes(t, reader)["queue_full"]; got != 1 {
		t.Fatalf("queue_full outcomes=%d want 1", got)
	}
	close(store.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gw.issueAI.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := issueAIOutcomes(t, reader)["failed"]; got < 1 {
		t.Fatalf("failed outcomes=%d want >=1", got)
	}
	if !strings.Contains(logs.String(), "metadata offline") {
		t.Fatalf("failure log does not carry the cause:\n%s", logs.String())
	}
}

func issueAIOutcomes(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "lazy_observe.issue_ai.automatic_outcomes" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("automatic_outcomes is %T, want an int64 counter", m.Data)
			}
			for _, point := range sum.DataPoints {
				outcome, _ := point.Attributes.Value("outcome")
				outcomes[outcome.AsString()] += point.Value
			}
		}
	}
	return outcomes
}
