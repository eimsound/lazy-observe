package errortrack

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/metric"
)

const inboxInstrumentationName = "github.com/eimsound/lazy-observe/internal/errortrack"

type inboxMetrics struct {
	registration metric.Registration
}

func newInboxMetrics(
	provider metric.MeterProvider,
	store InboxStore,
	now func() time.Time,
) (*inboxMetrics, error) {
	if provider == nil {
		return nil, nil
	}
	meter := provider.Meter(inboxInstrumentationName)
	activeItems, err := meter.Int64ObservableGauge(
		"lazy_observe.error_inbox.active_items",
		metric.WithDescription("Durably accepted error events awaiting completion"),
	)
	if err != nil {
		return nil, err
	}
	activeBytes, err := meter.Int64ObservableGauge(
		"lazy_observe.error_inbox.active_bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Payload bytes retained by active error Inbox items"),
	)
	if err != nil {
		return nil, err
	}
	oldestAge, err := meter.Int64ObservableGauge(
		"lazy_observe.error_inbox.oldest_age_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest active error Inbox item"),
	)
	if err != nil {
		return nil, err
	}
	retries, err := meter.Int64ObservableCounter("lazy_observe.error_inbox.retries")
	if err != nil {
		return nil, err
	}
	rejections, err := meter.Int64ObservableCounter("lazy_observe.error_inbox.rejections")
	if err != nil {
		return nil, err
	}
	terminalFailures, err := meter.Int64ObservableCounter("lazy_observe.error_inbox.terminal_failures")
	if err != nil {
		return nil, err
	}
	drops, err := meter.Int64ObservableCounter("lazy_observe.error_inbox.drops")
	if err != nil {
		return nil, err
	}
	registration, err := meter.RegisterCallback(
		func(ctx context.Context, observer metric.Observer) error {
			summary, summaryErr := store.ErrorInboxSummary(ctx)
			if summaryErr != nil {
				return summaryErr
			}
			var age int64
			if summary.ActiveItems > 0 {
				oldestAcceptedAt, oldestErr := store.OldestActiveErrorAcceptedAt(ctx)
				if oldestErr != nil {
					return oldestErr
				}
				if oldestAcceptedAt != nil {
					age = int64(now().UTC().Sub(oldestAcceptedAt.UTC()).Seconds())
					if age < 0 {
						age = 0
					}
				}
			}
			observer.ObserveInt64(activeItems, summary.ActiveItems)
			observer.ObserveInt64(activeBytes, summary.ActiveBytes)
			observer.ObserveInt64(oldestAge, age)
			observer.ObserveInt64(retries, summary.RetriesTotal)
			observer.ObserveInt64(rejections, summary.RejectionsTotal)
			observer.ObserveInt64(terminalFailures, summary.TerminalFailuresTotal)
			observer.ObserveInt64(drops, summary.DropsTotal)
			return nil
		},
		activeItems,
		activeBytes,
		oldestAge,
		retries,
		rejections,
		terminalFailures,
		drops,
	)
	if err != nil {
		return nil, err
	}
	return &inboxMetrics{registration: registration}, nil
}

func (m *inboxMetrics) Close() error {
	if m == nil || m.registration == nil {
		return nil
	}
	return m.registration.Unregister()
}
