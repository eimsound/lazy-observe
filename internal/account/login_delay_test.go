package account

import "testing"

func TestRandomLoginFailureDelayRange(t *testing.T) {
	for range 100 {
		delay := randomLoginFailureDelay()
		if delay < loginFailureMinDelay || delay > loginFailureMaxDelay {
			t.Fatalf("delay %s outside [%s, %s]", delay, loginFailureMinDelay, loginFailureMaxDelay)
		}
	}
}
