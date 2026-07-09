package vp8channel

import (
	"math"
	"testing"
)

// TestBrutalEffectiveBps pins the Hysteria "brutal" compensation formula and
// its clamps: effective = target/(1-loss), loss clamped to [0, 0.8], result
// clamped to [target, target*5]. Non-finite and out-of-range loss readings
// (including the 0/0 = NaN of an empty measurement window) must collapse to the
// target so the rate limiter is never handed 0 (uint32(NaN) == 0, which stalls
// the KCP send path — see brutalEffectiveBps docs).
func TestBrutalEffectiveBps(t *testing.T) {
	const target = 100_000 // bytes/sec

	tests := []struct {
		name string
		loss float64
		want int
	}{
		{"no loss returns target", 0, target},
		{"negative loss clamped to zero", -0.5, target},
		{"ten percent loss", 0.10, 111_111},   // 100000 / 0.9
		{"fifty percent loss", 0.50, 200_000}, // 100000 / 0.5
		{"loss clamped at 0.8 hits max mul", 0.80, target * 5},
		{"loss above clamp still capped at max mul", 0.95, target * 5},
		{"full loss clamps to max mul", 1.0, target * 5},
		{"loss above one clamps to max mul", 1.5, target * 5},
		{"NaN loss (empty window 0/0) returns target", math.NaN(), target},
		{"positive infinity loss returns target", math.Inf(1), target},
		{"negative infinity loss returns target", math.Inf(-1), target},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := brutalEffectiveBps(target, tc.loss)
			if got != tc.want {
				t.Fatalf("brutalEffectiveBps(%d, %v) = %d, want %d", target, tc.loss, got, tc.want)
			}
			// Invariant: with brutal enabled the result is always a finite,
			// positive value inside the clamp window — never 0, never NaN.
			if got < target || got > target*brutalMaxRateMul {
				t.Fatalf("result %d outside clamp [%d, %d]", got, target, target*brutalMaxRateMul)
			}
			if got <= 0 {
				t.Fatalf("result %d must be strictly positive when brutal is enabled", got)
			}
		})
	}
}

// TestBrutalEffectiveBpsDisabled verifies a non-positive target short-circuits
// (feature off), so no rate is ever computed for it.
func TestBrutalEffectiveBpsDisabled(t *testing.T) {
	if got := brutalEffectiveBps(0, 0.3); got != 0 {
		t.Fatalf("brutalEffectiveBps(0, 0.3) = %d, want 0", got)
	}
}
