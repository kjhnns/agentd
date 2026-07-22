package harness

import (
	"testing"
	"time"
)

// TestProxyPressureMaxDimension: proxy pressure is the LARGEST normalized ratio
// across turns/bytes/wallclock, clamped to [0,1], tagged source=proxy.
func TestProxyPressureMaxDimension(t *testing.T) {
	b := ProxyBudget{Turns: 100, Bytes: 1000, Wallclock: time.Hour}

	// Turns dimension dominates: 75/100 = 0.75; others lower.
	cp := b.Pressure(75, 100, time.Minute)
	if cp.Source != PressureProxy {
		t.Fatalf("source = %q, want proxy", cp.Source)
	}
	if !approx(cp.Fraction, 0.75) {
		t.Fatalf("turns-dominated fraction = %v, want 0.75", cp.Fraction)
	}

	// Bytes dimension dominates: 900/1000 = 0.90.
	cp = b.Pressure(10, 900, time.Minute)
	if !approx(cp.Fraction, 0.90) {
		t.Fatalf("bytes-dominated fraction = %v, want 0.90", cp.Fraction)
	}

	// Wallclock dimension dominates: 30m/60m = 0.50.
	cp = b.Pressure(1, 1, 30*time.Minute)
	if !approx(cp.Fraction, 0.50) {
		t.Fatalf("wallclock-dominated fraction = %v, want 0.50", cp.Fraction)
	}

	// Over budget clamps to 1.0.
	cp = b.Pressure(500, 100, time.Minute)
	if cp.Fraction != 1.0 {
		t.Fatalf("over-budget fraction = %v, want clamp 1.0", cp.Fraction)
	}
}

// TestProxyBudgetZeroDimensionSkipped: a zeroed budget dimension is ignored, so
// it cannot force pressure to 0 by division and cannot contribute a ratio.
func TestProxyBudgetZeroDimensionSkipped(t *testing.T) {
	b := ProxyBudget{Turns: 0, Bytes: 1000, Wallclock: 0}
	cp := b.Pressure(1_000_000, 500, time.Duration(1<<62))
	if !approx(cp.Fraction, 0.5) {
		t.Fatalf("fraction = %v, want 0.5 (only bytes dimension counts)", cp.Fraction)
	}
}

func TestDefaultProxyBudgetSane(t *testing.T) {
	b := DefaultProxyBudget()
	if b.Turns <= 0 || b.Bytes <= 0 || b.Wallclock <= 0 {
		t.Fatalf("default proxy budget has a non-positive dimension: %+v", b)
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
