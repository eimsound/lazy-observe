package issueai

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/ai"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/query"
)

type stubIssueQuerier struct{}

func (stubIssueQuerier) Query(
	_ context.Context,
	_ meta.ProjectInfo,
	sig query.Signal,
	_ string,
	_ url.Values,
) ([]byte, error) {
	if sig == query.SignalMetrics {
		return []byte(`{"status":"success","data":{"result":[]}}`), nil
	}
	return nil, nil
}

func (s stubIssueQuerier) QueryWithLimit(
	ctx context.Context,
	p meta.ProjectInfo,
	sig query.Signal,
	path string,
	params url.Values,
	_ int64,
) ([]byte, error) {
	return s.Query(ctx, p, sig, path, params)
}

func (stubIssueQuerier) MetricsStringList(context.Context, meta.ProjectInfo, string, url.Values) ([]string, error) {
	return nil, nil
}

type stubIssueStore struct {
	mu    sync.Mutex
	issue meta.IssueView
	saves int
}

func (s *stubIssueStore) GetIssue(context.Context, int, int) (meta.IssueView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issue, nil
}

func (s *stubIssueStore) SaveIssueAnalysis(
	_ context.Context,
	_, _ int,
	analysis map[string]any,
	at time.Time,
) (meta.IssueView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.issue.AIAnalysis = analysis
	s.issue.AIAnalysisAt = &at
	return s.issue, nil
}

type stubAIClient struct {
	responses []ai.ChatResponse
	ops       []string
}

func (c *stubAIClient) Chat(ctx context.Context, _ ai.ChatRequest) (ai.ChatResponse, error) {
	c.ops = append(c.ops, ai.OperationFromContext(ctx))
	resp := c.responses[0]
	c.responses = c.responses[1:]
	return resp, nil
}

func (*stubAIClient) ChatStream(context.Context, ai.ChatRequest) (ai.Stream, error) {
	return nil, errors.New("not implemented")
}

func TestGenerateAnalysisRetriesAndAggregatesUsage(t *testing.T) {
	client := &stubAIClient{
		responses: []ai.ChatResponse{
			{Content: "invalid", Usage: ai.Usage{InputTokens: 100, OutputTokens: 20}},
			{
				Content: `{"summary":"db timeout","probable_causes":[{"cause":"slow query","confidence":"high","evidence":"span"}],"suggested_actions":["add index"]}`,
				Usage:   ai.Usage{InputTokens: 130, OutputTokens: 40},
			},
		},
	}
	got, _, usage, err := GenerateAnalysis(
		context.Background(),
		ClientSnapshot{Enabled: true, Client: client},
		"<untrusted_telemetry>\nissue\n</untrusted_telemetry>",
	)
	if err != nil {
		t.Fatalf("GenerateAnalysis err=%v", err)
	}
	if got.Summary != "db timeout" || usage.InputTokens != 230 || usage.OutputTokens != 60 ||
		usage.TotalTokens() != 290 {
		t.Fatalf("got=%#v usage=%#v", got, usage)
	}
	if len(client.ops) != 2 || client.ops[0] != "issue_ai" {
		t.Fatalf("ops=%v", client.ops)
	}
}

func TestClaimEpochFencingRejectsStaleEpochAndLogsWarning(t *testing.T) {
	var logs bytes.Buffer
	store := &stubIssueStore{
		issue: meta.IssueView{ID: 42, ProjectID: 7, Fingerprint: "fp-42", Title: "boom"},
	}
	client := &stubAIClient{
		responses: []ai.ChatResponse{
			{
				Content: `{"summary":"ok","probable_causes":[{"cause":"c","confidence":"high","evidence":"e"}],"suggested_actions":["a"]}`,
				Usage:   ai.Usage{InputTokens: 10, OutputTokens: 5},
			},
		},
	}
	svc := New(Config{
		Issues:  store,
		Querier: stubIssueQuerier{},
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
	})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	key := issueKey{projectID: 7, issueID: 42}
	epoch1, release1, err := svc.acquireIssueLock(context.Background(), key)
	if err != nil || epoch1 != 1 {
		t.Fatalf("epoch1=%d err=%v", epoch1, err)
	}
	release1()

	epoch2, release2, err := svc.acquireIssueLock(context.Background(), key)
	if err != nil || epoch2 != 2 || svc.CurrentEpoch(7, 42) != 2 {
		t.Fatalf("epoch2=%d current=%d err=%v", epoch2, svc.CurrentEpoch(7, 42), err)
	}
	defer release2()

	// Attempting to publish with stale epoch1 must be rejected and logged with slog.Warn.
	_, _, _, err = svc.GenerateAndStore(
		context.Background(),
		ClientSnapshot{Enabled: true, Client: client},
		meta.ProjectInfo{ID: 7, Slug: "alpha"},
		store.issue,
		epoch1,
	)
	if !errors.Is(err, ErrStaleClaimEpoch) {
		t.Fatalf("err=%v want ErrStaleClaimEpoch", err)
	}
	if store.saves != 0 {
		t.Fatalf("stale epoch wrote %d saves, want 0", store.saves)
	}
	logLine := logs.String()
	for _, want := range []string{"stale claim epoch", "project_id=7", "issue_id=42", "claim_epoch=1", "current_epoch=2"} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("warning log missing %q:\n%s", want, logLine)
		}
	}
}
