package worker

import "time"

// Backoff returns how long to wait before the next try, after `attempt`
// failed tries (attempt starts at 1).
//
// The delay doubles each time (base, 2×base, 4×base, ...) up to max, so a
// struggling provider gets more and more room to recover. We then pick a
// random point in the upper half of that delay ("equal jitter"). Without the
// randomness, all messages that failed together during an outage would retry
// at the same moment and cause a new spike.
func Backoff(attempt int, base, max time.Duration, rand func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := max
	// Shifting by 30+ would overflow; by then we are long past max anyway.
	if attempt <= 30 {
		if exp := base << (attempt - 1); exp > 0 && exp < max {
			d = exp
		}
	}
	half := d / 2
	return half + time.Duration(rand()*float64(half))
}
