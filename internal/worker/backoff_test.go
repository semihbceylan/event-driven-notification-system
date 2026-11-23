package worker

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	base, max := 2*time.Second, 10*time.Minute
	low := func() float64 { return 0 }
	high := func() float64 { return 0.999999 }

	tests := []struct {
		attempt int
		full    time.Duration // un-jittered delay
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{5, 32 * time.Second},
		{9, 512 * time.Second},
		{10, max}, // 1024s would exceed max
		{64, max}, // must not overflow
	}
	for _, tt := range tests {
		lo := Backoff(tt.attempt, base, max, low)
		hi := Backoff(tt.attempt, base, max, high)
		if lo != tt.full/2 {
			t.Errorf("attempt %d: min delay = %v, want %v", tt.attempt, lo, tt.full/2)
		}
		if hi < tt.full/2 || hi > tt.full {
			t.Errorf("attempt %d: max delay = %v, want within [%v, %v]", tt.attempt, hi, tt.full/2, tt.full)
		}
	}
}
