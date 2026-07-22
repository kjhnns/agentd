package scheduler

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string, loc *time.Location) Schedule {
	t.Helper()
	s, err := ParseSchedule(spec, loc)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", spec, err)
	}
	return s
}

func TestEveryNextFire(t *testing.T) {
	s := mustParse(t, "@every 30m", time.UTC)
	after := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	got := s.Next(after)
	want := after.Add(30 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestDailyNextFireSameAndNextDay(t *testing.T) {
	s := mustParse(t, "@daily 07:00", time.UTC)

	// Before 07:00 -> today 07:00.
	after := time.Date(2026, 7, 22, 6, 30, 0, 0, time.UTC)
	if got, want := s.Next(after), time.Date(2026, 7, 22, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next(%v) = %v, want %v", after, got, want)
	}
	// Exactly 07:00 -> STRICTLY after, so tomorrow.
	after = time.Date(2026, 7, 22, 7, 0, 0, 0, time.UTC)
	if got, want := s.Next(after), time.Date(2026, 7, 23, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next(%v) = %v, want %v", after, got, want)
	}
}

func TestDailyTimezone(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	s := mustParse(t, "@daily 07:00", zurich)
	// 2026-07-22 06:00 UTC is 08:00 in Zurich (CEST, UTC+2): today's 07:00
	// Zurich already passed, so next fire is tomorrow 07:00 Zurich = 05:00 UTC.
	after := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	got := s.Next(after)
	want := time.Date(2026, 7, 23, 7, 0, 0, 0, zurich)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v (=%v UTC)", got, want, want.UTC())
	}
	if gotUTC := got.UTC().Hour(); gotUTC != 5 {
		t.Fatalf("Zurich 07:00 should be 05:00 UTC in July, got %d", gotUTC)
	}
}

func TestCronNextFire(t *testing.T) {
	cases := []struct {
		spec  string
		after time.Time
		want  time.Time
	}{
		// daily 06:30 via cron
		{"30 6 * * *", time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC), time.Date(2026, 7, 22, 6, 30, 0, 0, time.UTC)},
		{"30 6 * * *", time.Date(2026, 7, 22, 6, 45, 0, 0, time.UTC), time.Date(2026, 7, 23, 6, 30, 0, 0, time.UTC)},
		// Mondays 09:00 (2026-07-22 is a Wednesday; next Monday is 07-27)
		{"0 9 * * 1", time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC), time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)},
		// every 15 minutes
		{"*/15 * * * *", time.Date(2026, 7, 22, 10, 16, 0, 0, time.UTC), time.Date(2026, 7, 22, 10, 30, 0, 0, time.UTC)},
		// first of the month at midnight
		{"0 0 1 * *", time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		s := mustParse(t, c.spec, time.UTC)
		if got := s.Next(c.after); !got.Equal(c.want) {
			t.Errorf("%q Next(%v) = %v, want %v", c.spec, c.after, got, c.want)
		}
	}
}

func TestCronTimezone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	s := mustParse(t, "0 9 * * *", ny) // 09:00 New York daily
	after := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	got := s.Next(after)
	want := time.Date(2026, 7, 22, 9, 0, 0, 0, ny) // = 13:00 UTC (EDT)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestParseScheduleErrors(t *testing.T) {
	bad := []string{
		"@every", "@every nonsense", "@every -5m",
		"@daily", "@daily 25:00", "@daily 7", "@hourly",
		"* * * *", "* * * * * *", "61 * * * *", "* 24 * * *", "a b c d e",
	}
	for _, spec := range bad {
		if _, err := ParseSchedule(spec, time.UTC); err == nil {
			t.Errorf("ParseSchedule(%q) succeeded, want error", spec)
		}
	}
}
