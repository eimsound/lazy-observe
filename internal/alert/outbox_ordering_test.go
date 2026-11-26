package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
)

type orderedDelivery struct {
	status string
	cycle  string
}

// orderingEndpoint records error_rate_spike deliveries in arrival order and
// fails the first failures[status/cycle] requests of a transition with 503.
type orderingEndpoint struct {
	mu        sync.Mutex
	failures  map[orderedDelivery]int
	attempts  map[orderedDelivery]int
	delivered []orderedDelivery
	server    *httptest.Server
}

func newOrderingEndpoint(t *testing.T, failures map[orderedDelivery]int) *orderingEndpoint {
	t.Helper()
	e := &orderingEndpoint{failures: failures, attempts: make(map[orderedDelivery]int)}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil || len(n.Alerts) != 1 {
			http.Error(w, "bad notification", http.StatusBadRequest)
			return
		}
		key := orderedDelivery{status: n.Status, cycle: n.Alerts[0].Annotations["cycle"]}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.attempts[key]++
		if e.attempts[key] <= e.failures[key] {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		e.delivered = append(e.delivered, key)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *orderingEndpoint) snapshot() ([]orderedDelivery, map[orderedDelivery]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	attempts := make(map[orderedDelivery]int, len(e.attempts))
	for k, v := range e.attempts {
		attempts[k] = v
	}
	return append([]orderedDelivery(nil), e.delivered...), attempts
}

// enqueueSpikeCycles enqueues firing(1), resolved(1), firing(2) for one
// platform alert stream, all due at once, the way a crash-recovery drain or a
// slow worker leaves them.
func enqueueSpikeCycles(t *testing.T, store *meta.Store, projectID, channelID int) []int64 {
	t.Helper()
	var ids []int64
	for _, ref := range []PlatformStateRef{
		{ProjectID: projectID, Kind: "error_rate_spike", Cycle: 1, Status: "firing"},
		{ProjectID: projectID, Kind: "error_rate_spike", Cycle: 1, Status: "resolved"},
		{ProjectID: projectID, Kind: "error_rate_spike", Cycle: 2, Status: "firing"},
	} {
		n := Notification{Project: "ordering", Rule: "error_rate_spike", Status: ref.Status, Alerts: []Alert{{
			Status:      ref.Status,
			Annotations: map[string]string{"cycle": strconv.FormatInt(ref.Cycle, 10)},
		}}}
		for _, entry := range BuildPlatformStateOutboxEntries(ref, []int{channelID}, []Notification{n}) {
			id, inserted, err := store.EnqueueNotification(context.Background(), entry)
			if err != nil || !inserted {
				t.Fatalf("enqueue %+v inserted=%v err=%v", ref, inserted, err)
			}
			ids = append(ids, id)
		}
	}
	return ids
}

func startOrderingWorkers(t *testing.T, store *meta.Store, endpoint *orderingEndpoint, maxAttempts int) {
	t.Helper()
	for _, id := range []string{"ordering-a", "ordering-b"} {
		worker := NewOutboxWorker(OutboxWorkerConfig{
			Store:        store,
			Sender:       &Sender{Client: endpoint.server.Client(), Sleep: func(time.Duration) {}},
			WorkerID:     id,
			PollInterval: 5 * time.Millisecond,
			RetryBase:    time.Millisecond,
			MaxAttempts:  maxAttempts,
			ClaimBatch:   8,
		})
		worker.Start()
		t.Cleanup(worker.Close)
		worker.Wake()
	}
}

func setupOrderingStore(t *testing.T, endpoint *orderingEndpoint) (*meta.Store, int, int) {
	t.Helper()
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "Ordering", "ordering")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "webhook", Name: "ordering-hook", Config: map[string]any{"url": endpoint.server.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, project.ID, channel.ID
}

// Issue #386: two competing workers with concurrent batch delivery must still
// deliver one alert's transitions in causal order, even when the firing
// delivery fails transiently and backs off while the higher-priority resolved
// is already due.
func TestOutboxWorkersDeliverPlatformStreamInCausalOrder(t *testing.T) {
	endpoint := newOrderingEndpoint(t, map[orderedDelivery]int{{status: "firing", cycle: "1"}: 1})
	store, projectID, channelID := setupOrderingStore(t, endpoint)
	ids := enqueueSpikeCycles(t, store, projectID, channelID)
	startOrderingWorkers(t, store, endpoint, 5)
	waitOutboxStatus(t, store, ids[2], "delivered", 5*time.Second)
	delivered, attempts := endpoint.snapshot()
	want := []orderedDelivery{{"firing", "1"}, {"resolved", "1"}, {"firing", "2"}}
	if len(delivered) != len(want) {
		t.Fatalf("delivered=%v want %v", delivered, want)
	}
	for i := range want {
		if delivered[i] != want[i] {
			t.Fatalf("delivery order=%v want %v", delivered, want)
		}
	}
	if attempts[orderedDelivery{"firing", "1"}] != 2 {
		t.Fatalf("firing attempts=%d want 2 (one transient failure)", attempts[orderedDelivery{"firing", "1"}])
	}
}

// A firing delivery that exhausts its retries (dead) releases the resolved
// behind it instead of blocking the stream forever.
func TestOutboxWorkerDeadPredecessorReleasesSuccessor(t *testing.T) {
	endpoint := newOrderingEndpoint(t, map[orderedDelivery]int{{status: "firing", cycle: "1"}: 100})
	store, projectID, channelID := setupOrderingStore(t, endpoint)
	ids := enqueueSpikeCycles(t, store, projectID, channelID)
	startOrderingWorkers(t, store, endpoint, 2)
	waitOutboxStatus(t, store, ids[0], "dead", 5*time.Second)
	waitOutboxStatus(t, store, ids[2], "delivered", 5*time.Second)
	delivered, attempts := endpoint.snapshot()
	want := []orderedDelivery{{"resolved", "1"}, {"firing", "2"}}
	if len(delivered) != len(want) || delivered[0] != want[0] || delivered[1] != want[1] {
		t.Fatalf("delivered=%v want %v", delivered, want)
	}
	if attempts[orderedDelivery{"firing", "1"}] != 2 || attempts[orderedDelivery{"resolved", "1"}] != 1 {
		t.Fatalf("attempts=%v", attempts)
	}
}

// Native rule and platform state rows join their causal stream; storm
// summaries and issue lifecycle rows stay unordered.
func TestOutboxEntriesCarryCausalOrderingKeys(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	firing := Notification{Project: "p", Rule: "r", Status: "firing", Alerts: []Alert{{StartsAt: now}}}
	resolved := Notification{Project: "p", Rule: "r", Status: "resolved", Alerts: []Alert{{StartsAt: now}}}
	storm := Notification{Project: "p", Rule: "alert_storm", Status: "firing", Storm: &StormSummary{GroupCount: 3}}
	ruleID := 7
	cases := []struct {
		name    string
		entries []meta.NotificationOutboxEnqueue
		want    []string
	}{
		{
			name: "native rule pass",
			entries: BuildRuleOutboxEntries(1, ruleID, []int{3, 4}, []Notification{firing, resolved, storm}, now,
				time.Hour, time.Hour),
			want: []string{"rule:7", "rule:7", "rule:7", "rule:7", "", ""},
		},
		{
			name:    "rule update resolution",
			entries: BuildOutboxEntries(1, &ruleID, []int{3}, []Notification{resolved}, now, time.Hour),
			want:    []string{"rule:7"},
		},
		{
			name:    "storm without rule",
			entries: BuildOutboxEntries(1, nil, []int{3}, []Notification{storm}, now, time.Hour),
			want:    []string{""},
		},
		{
			name: "issue lifecycle",
			entries: BuildOutboxEntriesWithLifecycle(1, nil, 99, []int{3}, []Notification{{Project: "p",
				Rule: "new_issue", Status: "firing"}}, now, time.Hour),
			want: []string{""},
		},
		{
			name: "platform state",
			entries: BuildPlatformStateOutboxEntries(
				PlatformStateRef{ProjectID: 5, Kind: "error_rate_spike", Cycle: 2, Status: "resolved"},
				[]int{3},
				[]Notification{resolved},
			),
			want: []string{"platform-state:5:error_rate_spike"},
		},
	}
	for _, tc := range cases {
		if len(tc.entries) != len(tc.want) {
			t.Fatalf("%s: entries=%d want %d", tc.name, len(tc.entries), len(tc.want))
		}
		for i, entry := range tc.entries {
			if entry.OrderingKey != tc.want[i] {
				t.Fatalf("%s: entry %d ordering key=%q want %q", tc.name, i, entry.OrderingKey, tc.want[i])
			}
		}
	}
}
