// Context-pressure signal (design: session lifecycle + context reset). agentd
// does NOT compact; it owns RESET. To decide WHEN to reset, the core needs a
// harness-agnostic read of how full the live context window is. That read is a
// normalized ContextPressure: a fraction 0..1 of the window in use, tagged with
// its SOURCE so the core knows whether the number is measured or estimated.
//
//   - real:  the adapter reports true token usage (Claude Code parses the
//     stream-json usage blocks: input + cache_read + cache_creation +
//     output, divided by the model context window).
//   - proxy: no usage is available, so pressure is ESTIMATED from a proxy of
//     cumulative turns / bytes exchanged / wall-clock since start. A
//     PTY or a not-yet-instrumented adapter uses this; the Claude
//     adapter also uses it as a backstop before the first usage line.
//
// Proxy pressure is deliberately conservative-by-max: it takes the LARGEST of
// the three normalized ratios so any single runaway dimension (a very long
// session, a very chatty one, a very old one) still trips a backstop reset.
package harness

import "time"

// PressureSource records whether a pressure figure is measured or estimated.
type PressureSource string

const (
	PressureReal  PressureSource = "real"  // measured from real token usage
	PressureProxy PressureSource = "proxy" // estimated from turns/bytes/wallclock
)

// ContextPressure is the normalized fraction (0..1) of the context window in
// use for a live session, plus the source of the figure. Tokens/Window are set
// only when Source == real (they are the measured numerator and the window used
// as denominator).
type ContextPressure struct {
	Fraction float64        `json:"fraction"`
	Source   PressureSource `json:"source"`
	Tokens   int            `json:"tokens,omitempty"`
	Window   int            `json:"window,omitempty"`
}

// ProxyBudget bounds each proxy dimension; a session at (or past) any budget is
// treated as full on that dimension. Pressure is the max ratio across the three.
type ProxyBudget struct {
	Turns     int           // turns before proxy pressure hits 1.0 on the turns axis
	Bytes     int64         // bytes exchanged before proxy pressure hits 1.0
	Wallclock time.Duration // session age before proxy pressure hits 1.0
}

// DefaultProxyBudget is a sane default for a code-agent session: a few hundred
// turns, a few MB of I/O, or a couple of hours all read as "context likely
// full" for a proxy-only harness. These are backstops, not tuned estimates; a
// real-usage adapter should be preferred whenever available.
func DefaultProxyBudget() ProxyBudget {
	return ProxyBudget{
		Turns:     200,
		Bytes:     8 * 1024 * 1024,
		Wallclock: 2 * time.Hour,
	}
}

// Pressure estimates context pressure from proxy signals. The result is the
// LARGEST normalized ratio across the enabled dimensions (a zeroed budget
// dimension is skipped), clamped to [0,1], tagged source=proxy.
func (b ProxyBudget) Pressure(turns int, bytes int64, elapsed time.Duration) ContextPressure {
	frac := 0.0
	if b.Turns > 0 {
		frac = maxf(frac, float64(turns)/float64(b.Turns))
	}
	if b.Bytes > 0 {
		frac = maxf(frac, float64(bytes)/float64(b.Bytes))
	}
	if b.Wallclock > 0 {
		frac = maxf(frac, float64(elapsed)/float64(b.Wallclock))
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return ContextPressure{Fraction: frac, Source: PressureProxy}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
