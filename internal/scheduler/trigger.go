// Trigger model + schedule-spec parsing for the scheduler (dependency-free).
//
// TRIGGER TAXONOMY (v1):
//
//   - schedule: cron-like time trigger. Spec forms:
//     "@every DUR"      e.g. "@every 30m", "@every 1h" (Go duration)
//     "@daily HH:MM"    e.g. "@daily 07:00"
//     "M H DOM MON DOW" basic 5-field cron, e.g. "30 6 * * *", "0 9 * * 1".
//     Fields support *, N, N-M, */S, N-M/S and comma lists. DOW is 0-6
//     (0=Sunday; 7 also accepted as Sunday). When both DOM and DOW are
//     restricted, standard cron OR semantics apply.
//     TIMEZONE: a job may set tz = "Europe/Zurich" (IANA name); the default is
//     the server's LOCAL timezone. "@every" is timezone-independent.
//   - file: a path watcher. Fires when anything under the watched path changes
//     (poll-based mtime/size/count signature; no fsnotify dependency).
//   - webhook: an authenticated local API endpoint (POST /hooks/:job) fires the
//     job. Gated by the same API bearer as the rest of the API.
//   - message: NOT a job trigger. Inbound channel messages already drive turns
//     via session.RouteInbound; they are listed here only to complete the
//     taxonomy. Configuring trigger = "message" is rejected.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TriggerKind enumerates the v1 trigger kinds.
type TriggerKind string

const (
	TriggerSchedule TriggerKind = "schedule"
	TriggerFile     TriggerKind = "file"
	TriggerWebhook  TriggerKind = "webhook"
)

// Trigger is a parsed job trigger.
type Trigger struct {
	Kind TriggerKind
	Spec string // the raw schedule spec (schedule) or watched path (file)

	Schedule Schedule      // parsed schedule (Kind == schedule)
	Path     string        // watched path (Kind == file)
	Poll     time.Duration // file poll interval (Kind == file; default 10s)
	TZ       string        // IANA tz name for display; empty = server local
}

// String renders the trigger for lists/logs.
func (t Trigger) String() string {
	switch t.Kind {
	case TriggerSchedule:
		if t.TZ != "" {
			return fmt.Sprintf("schedule %q tz=%s", t.Spec, t.TZ)
		}
		return fmt.Sprintf("schedule %q", t.Spec)
	case TriggerFile:
		return fmt.Sprintf("file %q poll=%s", t.Path, t.Poll)
	case TriggerWebhook:
		return "webhook (POST /hooks/:job)"
	}
	return string(t.Kind)
}

// Schedule computes fire times. Next returns the first occurrence STRICTLY
// after the given instant (zero time if none within the search horizon).
type Schedule interface {
	Next(after time.Time) time.Time
}

// ParseSchedule parses a schedule spec. loc is the timezone for "@daily" and
// cron specs (nil = time.Local); "@every" ignores it.
func ParseSchedule(spec string, loc *time.Location) (Schedule, error) {
	if loc == nil {
		loc = time.Local
	}
	spec = strings.TrimSpace(spec)
	switch {
	case strings.HasPrefix(spec, "@every "):
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(spec, "@every ")))
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", spec, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("schedule %q: interval must be positive", spec)
		}
		return everySchedule{d}, nil
	case strings.HasPrefix(spec, "@daily "):
		hm := strings.TrimSpace(strings.TrimPrefix(spec, "@daily "))
		parts := strings.SplitN(hm, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("schedule %q: want @daily HH:MM", spec)
		}
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return nil, fmt.Errorf("schedule %q: want @daily HH:MM (00:00..23:59)", spec)
		}
		return dailySchedule{hour: h, min: m, loc: loc}, nil
	case strings.HasPrefix(spec, "@"):
		return nil, fmt.Errorf("schedule %q: unknown @-form (want @every or @daily)", spec)
	default:
		return parseCron(spec, loc)
	}
}

// everySchedule fires every fixed interval, anchored to the previous fire.
type everySchedule struct{ d time.Duration }

func (s everySchedule) Next(after time.Time) time.Time { return after.Add(s.d) }

// dailySchedule fires once a day at HH:MM in loc.
type dailySchedule struct {
	hour, min int
	loc       *time.Location
}

func (s dailySchedule) Next(after time.Time) time.Time {
	day := after.In(s.loc)
	for i := 0; i < 4; i++ { // extra iterations absorb DST day-boundary shifts
		cand := time.Date(day.Year(), day.Month(), day.Day(), s.hour, s.min, 0, 0, s.loc)
		if cand.After(after) {
			return cand
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}
}

// cronSchedule is a basic 5-field cron: minute hour dom month dow.
type cronSchedule struct {
	min, hour, dom, mon, dow [64]bool
	domStar, dowStar         bool
	loc                      *time.Location
}

func parseCron(spec string, loc *time.Location) (Schedule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule %q: want 5 cron fields (M H DOM MON DOW) or an @every/@daily form", spec)
	}
	c := &cronSchedule{loc: loc}
	var err error
	if c.min, _, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("schedule %q minute: %w", spec, err)
	}
	if c.hour, _, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("schedule %q hour: %w", spec, err)
	}
	if c.dom, c.domStar, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("schedule %q day-of-month: %w", spec, err)
	}
	if c.mon, _, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("schedule %q month: %w", spec, err)
	}
	if c.dow, c.dowStar, err = parseCronField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("schedule %q day-of-week: %w", spec, err)
	}
	if c.dow[7] { // 7 = Sunday alias
		c.dow[0] = true
	}
	return c, nil
}

func parseCronField(f string, lo, hi int) ([64]bool, bool, error) {
	var set [64]bool
	star := false
	for _, part := range strings.Split(f, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return set, false, fmt.Errorf("empty list element")
		}
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s <= 0 {
				return set, false, fmt.Errorf("bad step %q", part)
			}
			step = s
			part = part[:i]
		}
		start, end := lo, hi
		switch {
		case part == "*":
			if step == 1 && f == "*" {
				star = true
			}
		case strings.Contains(part, "-"):
			ab := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(ab[0])
			b, err2 := strconv.Atoi(ab[1])
			if err1 != nil || err2 != nil || a > b {
				return set, false, fmt.Errorf("bad range %q", part)
			}
			start, end = a, b
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return set, false, fmt.Errorf("bad value %q", part)
			}
			start, end = n, n
		}
		if start < lo || end > hi {
			return set, false, fmt.Errorf("value out of range %d-%d in %q", lo, hi, part)
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	return set, star, nil
}

// Next scans minute-by-minute (bounded to ~2 years); plenty fast for a
// per-tick computation and keeps the matcher trivially correct.
func (c *cronSchedule) Next(after time.Time) time.Time {
	t := after.In(c.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(2, 0, 1)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if !c.mon[int(t.Month())] {
			continue
		}
		domOK := c.dom[t.Day()]
		dowOK := c.dow[int(t.Weekday())]
		// Standard cron: if both DOM and DOW are restricted, either matches.
		var dayOK bool
		switch {
		case c.domStar && c.dowStar:
			dayOK = true
		case c.domStar:
			dayOK = dowOK
		case c.dowStar:
			dayOK = domOK
		default:
			dayOK = domOK || dowOK
		}
		if !dayOK {
			continue
		}
		if c.hour[t.Hour()] && c.min[t.Minute()] {
			return t
		}
	}
	return time.Time{}
}
