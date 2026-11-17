// Package backoff provides overflow-safe delay calculation. It deliberately
// owns no clocks, sleeping, randomness, attempt budgets, or persistence.
package backoff

import (
	"errors"
	"math"
	"time"
)

// Delay returns an equal-jitter exponential delay for a zero-based retry
// index. A positive server delay takes precedence and is bounded by serverCap.
func Delay(
	retryIndex int,
	initial time.Duration,
	cap time.Duration,
	jitterSample float64,
	serverDelay time.Duration,
	serverCap time.Duration,
) (time.Duration, error) {
	if retryIndex < 0 || initial <= 0 || cap < initial {
		return 0, errors.New("retry index, initial delay, and cap are invalid")
	}
	if math.IsNaN(jitterSample) || jitterSample < 0 || jitterSample > 1 {
		return 0, errors.New("jitter sample must be between zero and one")
	}
	if serverDelay < 0 || serverCap < 0 || (serverDelay > 0 && serverCap == 0) {
		return 0, errors.New("server delay and cap are invalid")
	}
	if serverDelay > 0 {
		if serverDelay > serverCap {
			return serverCap, nil
		}
		return serverDelay, nil
	}

	maximum := initial
	for retry := 0; retry < retryIndex; retry++ {
		if maximum > cap-maximum {
			maximum = cap
			break
		}
		maximum *= 2
	}
	if maximum > cap {
		maximum = cap
	}
	half := maximum / 2
	if jitterSample == 0 {
		return half, nil
	}
	if jitterSample == 1 {
		return maximum, nil
	}
	return half + time.Duration(float64(maximum-half)*jitterSample), nil
}
