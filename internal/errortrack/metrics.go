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
			diagnostics, diagnosticsErr := store.ErrorInboxDiagnostics(ctx, 1)
			if diagnosticsErr != nil {
				return diagnosticsErr
			}
			observer.ObserveInt64(activeItems, diagnostics.ActiveItems)
			observer.ObserveInt64(activeBytes, diagnostics.ActiveBytes)
			var age int64
			if diagnostics.OldestAcceptedAt != nil {
				age = int64(now().UTC().Sub(diagnostics.OldestAcceptedAt.UTC()).Seconds())
				if age < 0 {
					age = 0
				}
			}
			observer.ObserveInt64(oldestAge, age)
			observer.ObserveInt64(retries, diagnostics.RetriesTotal)
			observer.ObserveInt64(rejections, diagnostics.RejectionsTotal)
			observer.ObserveInt64(terminalFailures, diagnostics.TerminalFailuresTotal)
			observer.ObserveInt64(drops, diagnostics.DropsTotal)
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
