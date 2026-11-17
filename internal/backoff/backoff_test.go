package backoff

import (
	"math"
	"testing"
	"time"
)

func TestDelayTable(t *testing.T) {
	tests := []struct {
		name                            string
		retry                           int
		initial, cap, server, serverCap time.Duration
		jitter                          float64
		want                            time.Duration
		wantErr                         bool
	}{
		{
			name:    "first lower equal-jitter bound",
			retry:   0,
			initial: time.Second,
			cap:     time.Minute,
			jitter:  0,
			want:    500 * time.Millisecond,
		},
		{
			name:    "first upper equal-jitter bound",
			retry:   0,
			initial: time.Second,
			cap:     time.Minute,
			jitter:  1,
			want:    time.Second,
		},
		{name: "third retry", retry: 2, initial: time.Second, cap: time.Minute, jitter: 1, want: 4 * time.Second},
		{
			name:    "exponential saturation",
			retry:   math.MaxInt,
			initial: time.Second,
			cap:     5 * time.Minute,
			jitter:  1,
			want:    5 * time.Minute,
		},
		{
			name:    "duration overflow saturation",
			retry:   62,
			initial: time.Hour,
			cap:     time.Duration(math.MaxInt64),
			jitter:  1,
			want:    time.Duration(math.MaxInt64),
		},
		{
			name:      "server delay wins",
			retry:     4,
			initial:   time.Second,
			cap:       time.Minute,
			jitter:    0,
			server:    45 * time.Second,
			serverCap: 2 * time.Minute,
			want:      45 * time.Second,
		},
		{
			name:      "server delay capped",
			retry:     0,
			initial:   time.Second,
			cap:       time.Minute,
			jitter:    1,
			server:    5 * time.Minute,
			serverCap: 2 * time.Minute,
			want:      2 * time.Minute,
		},
		{name: "negative retry", retry: -1, initial: time.Second, cap: time.Minute, jitter: 1, wantErr: true},
		{name: "zero initial", retry: 0, cap: time.Minute, jitter: 1, wantErr: true},
		{name: "cap below initial", retry: 0, initial: time.Minute, cap: time.Second, jitter: 1, wantErr: true},
		{name: "jitter below zero", retry: 0, initial: time.Second, cap: time.Minute, jitter: -0.1, wantErr: true},
		{name: "jitter above one", retry: 0, initial: time.Second, cap: time.Minute, jitter: 1.1, wantErr: true},
		{
			name:    "server delay without cap",
			retry:   0,
			initial: time.Second,
			cap:     time.Minute,
			jitter:  1,
			server:  time.Second,
			wantErr: true,
		},
		{
			name:      "negative server delay",
			retry:     0,
			initial:   time.Second,
			cap:       time.Minute,
			jitter:    1,
			server:    -time.Second,
			serverCap: time.Minute,
			wantErr:   true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Delay(test.retry, test.initial, test.cap, test.jitter, test.server, test.serverCap)
			if test.wantErr {
				if err == nil {
					t.Fatalf("Delay()=%s, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("Delay()=%s err=%v want=%s", got, err, test.want)
			}
		})
	}
}
