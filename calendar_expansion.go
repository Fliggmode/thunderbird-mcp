package main

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/mudler/thunderbird-mcp/internal/rrule"
	"strings"
	"time"
	_ "time/tzdata"
)

// Thunderbird constants: see docs/calendar.md for immutable upstream sources.
const calendarAllDay = 8
const calendarHasRecurrence = 16
const calendarOfflineDeleted = 4
const calendarMaxRows = 10000
const calendarMaxCandidates = 50000
const calendarMaxWork = 100000
const calendarMaxRuleBytes = 8192

type calendarBudget struct{ rows, candidates, work int }

func (b *calendarBudget) row(ctx context.Context) error {
	b.rows++
	if b.rows > calendarMaxRows {
		return fmt.Errorf("calendar row budget exceeded (%d)", calendarMaxRows)
	}
	return ctx.Err()
}
func (b *calendarBudget) candidate(ctx context.Context) error {
	b.candidates++
	if b.candidates > calendarMaxCandidates {
		return fmt.Errorf("calendar candidate budget exceeded (%d)", calendarMaxCandidates)
	}
	return ctx.Err()
}
func (b *calendarBudget) step(ctx context.Context) error {
	b.work++
	if b.work > calendarMaxWork {
		return fmt.Errorf("calendar work budget exceeded (%d)", calendarMaxWork)
	}
	return ctx.Err()
}
func calendarZone(tz string) (*time.Location, error) {
	// Thunderbird strips its historical /mozilla.org/<dtstamp>/ prefix.
	if strings.HasPrefix(tz, "/mozilla.org/") {
		_, tail, ok := strings.Cut(strings.TrimPrefix(tz, "/mozilla.org/"), "/")
		if !ok {
			return nil, fmt.Errorf("invalid legacy Thunderbird timezone")
		}
		tz = tail
	}
	if tz == "Z" {
		tz = "UTC"
	}

	if tz == "" || tz == "floating" || tz == "UTC" {
		return time.UTC, nil
	}
	if strings.Contains(tz, "VTIMEZONE") {
		return nil, fmt.Errorf("unsupported embedded VTIMEZONE")
	}
	loc, e := time.LoadLocation(tz)
	if e != nil {
		return nil, fmt.Errorf("unsupported calendar timezone %q: %w", tz, e)
	}
	return loc, nil
}
func calendarOverlap(v storedCalendarEvent, a, b time.Time) bool {
	if v.deleted || !v.event.Start.Before(b) {
		return false
	}
	if v.event.End.Equal(v.event.Start) {
		return !v.event.Start.Before(a)
	}
	return v.event.End.After(a)
}
func prepareCalendarEvent(v storedCalendarEvent) (storedCalendarEvent, error) {
	loc, e := calendarZone(v.startTZ)
	if e != nil {
		return v, e
	}
	endloc, e := calendarZone(v.endTZ)
	if e != nil {
		return v, e
	}
	v.event.Start = v.event.Start.In(loc)
	v.event.End = v.event.End.In(endloc)
	if v.event.End.Before(v.event.Start) {
		return v, fmt.Errorf("calendar event has negative duration")
	}
	return v, nil
}
func calendarRules(ctx context.Context, b *calendarBudget, v storedCalendarEvent) ([]string, error) {
	rows, e := v.source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='cal_recurrence'`)
	if e != nil {
		return nil, e
	}
	exists := rows.Next()
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if !exists {
		if v.flags&calendarHasRecurrence != 0 {
			return nil, fmt.Errorf("calendar recurrence table missing")
		}
		return nil, nil
	}
	rows, e = v.source.QueryContext(ctx, `SELECT icalString FROM cal_recurrence WHERE cal_id=? AND item_id=? LIMIT 10001`, v.key.calID, v.key.uid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		if e = b.row(ctx); e != nil {
			return nil, e
		}
		var s string
		if e = rows.Scan(&s); e != nil {
			return nil, e
		}
		if len(s) > calendarMaxRuleBytes {
			return nil, fmt.Errorf("calendar recurrence byte budget exceeded")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func expandCalendar(ctx context.Context, b *calendarBudget, merged map[calendarIdentity]storedCalendarEvent, since, until time.Time) ([]storedCalendarEvent, error) {
	var out []storedCalendarEvent
	exceptions := map[string]map[int64]storedCalendarEvent{}
	group := func(k calendarIdentity) string { return fmt.Sprintf("%d:%s%s", len(k.calID), k.calID, k.uid) }
	for k, v := range merged {
		if e := b.step(ctx); e != nil {
			return nil, e
		}
		var e error
		v, e = prepareCalendarEvent(v)
		if e != nil {
			return nil, e
		}
		merged[k] = v
		if k.recurrence.Valid {
			g := group(k)
			if exceptions[g] == nil {
				exceptions[g] = map[int64]storedCalendarEvent{}
			}
			exceptions[g][k.recurrence.Int64] = v
		}
	}
	for k, v := range merged {
		if k.recurrence.Valid { // Detached rows are emitted separately, including moved-in instances.
			master := k
			master.recurrence = sql.NullInt64{}
			master.timezone = sql.NullString{}
			if m, ok := merged[master]; ok && m.deleted {
				continue
			}
			if calendarOverlap(v, since, until) {
				out = append(out, v)
			}
			continue
		}
		if v.deleted {
			continue
		}
		rules, e := calendarRules(ctx, b, v)
		if e != nil {
			return nil, e
		}
		starts := map[int64]time.Time{v.event.Start.UnixMicro(): v.event.Start}
		excluded := map[int64]bool{}
		for _, line := range rules {
			line = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(line, "\r\n ", ""), "\r\n\t", ""))
			head, value, ok := strings.Cut(line, ":")
			if !ok {
				return nil, fmt.Errorf("invalid calendar recurrence property")
			}
			name := strings.Split(head, ";")[0]
			switch name {
			case "RRULE":
				if head != "RRULE" {
					return nil, fmt.Errorf("unsupported RRULE parameters")
				}
				if e := validateCalendarRule(value); e != nil {
					return nil, e
				}
				opt, e := rrule.StrToROptionInLocation(value, v.event.Start.Location())
				if e != nil {
					return nil, fmt.Errorf("calendar RRULE: %w", e)
				}
				if opt.Freq > rrule.DAILY || len(opt.Byeaster) > 0 || opt.Interval < 0 || opt.Interval > 10000 || opt.Count < 0 {
					return nil, fmt.Errorf("unsupported calendar RRULE frequency, interval, count, or BYEASTER")
				}
				// Each instrumented period scans at most 366 days and one time per day.
				// Subdaily frequencies have additional internal search loops: reject them.
				if len(opt.Byhour) > 1 || len(opt.Byminute) > 1 || len(opt.Bysecond) > 1 || len(opt.Bysetpos) > 366 {
					return nil, fmt.Errorf("unsupported calendar RRULE time expansion")
				}
				if len(opt.Bysecond) == 1 && opt.Bysecond[0] == 60 {
					return nil, fmt.Errorf("unsupported leap second")
				}
				if v.event.Start.Year() < 1 || v.event.Start.Year() > 9999 {
					return nil, fmt.Errorf("unsupported recurrence year")
				}
				opt.Dtstart = v.event.Start
				if opt.Until.IsZero() || opt.Until.After(until) {
					opt.Until = until
				}
				r, e := rrule.NewRRule(*opt)
				if e != nil {
					return nil, e
				}
				r.Work = func() error { return b.step(ctx) }
				next := r.Iterator()
				for {
					dt, ok := next()
					if !ok {
						break
					}
					if e = b.candidate(ctx); e != nil {
						return nil, e
					}
					if !dt.Before(until) {
						break
					}
					hour, minute, second := v.event.Start.Clock()
					if len(opt.Byhour) > 0 {
						hour = opt.Byhour[0]
					}
					if len(opt.Byminute) > 0 {
						minute = opt.Byminute[0]
					}
					if len(opt.Bysecond) > 0 {
						second = opt.Bysecond[0]
					}
					if dt.Hour() != hour || dt.Minute() != minute || dt.Second() != second {
						return nil, fmt.Errorf("unsupported recurrence in a DST gap")
					}
					if err := calendarCivilTime(dt); err != nil {
						return nil, err
					}
					starts[dt.UnixMicro()] = dt
				}
				if r.Err != nil {
					return nil, r.Err
				}
			case "RDATE", "EXDATE":
				dates, e := calendarDates(head, value, v.event.Start.Location())
				if e != nil {
					return nil, e
				}
				for _, dt := range dates {
					if e = b.candidate(ctx); e != nil {
						return nil, e
					}
					if name == "EXDATE" {
						excluded[dt.UnixMicro()] = true
					} else {
						starts[dt.UnixMicro()] = dt
					}
				}
			default:
				return nil, fmt.Errorf("unsupported calendar recurrence property %q", name)
			}
		}
		for id, dt := range starts {
			if e := b.candidate(ctx); e != nil {
				return nil, e
			}
			if excluded[id] {
				continue
			}
			if _, ok := exceptions[group(k)][id]; ok {
				continue
			}
			occ := v
			occ.event.Start = dt
			occ.event.End = dt.Add(v.event.End.Sub(v.event.Start))
			if v.flags&calendarAllDay != 0 {
				a, z := v.event.Start, v.event.End
				days := int(time.Date(z.Year(), z.Month(), z.Day(), 0, 0, 0, 0, time.UTC).Sub(time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)) / (24 * time.Hour))
				occ.event.End = dt.AddDate(0, 0, days)
			}
			occ.key.recurrence = sql.NullInt64{Int64: id, Valid: len(rules) > 0}
			if calendarOverlap(occ, since, until) {
				out = append(out, occ)
			}
		}
	}
	return out, nil
}
func calendarDates(head, value string, loc *time.Location) ([]time.Time, error) {
	dateOnly := false
	for _, p := range strings.Split(head, ";")[1:] {
		switch {
		case p == "VALUE=DATE":
			dateOnly = true
		case p == "VALUE=DATE-TIME":
		case strings.HasPrefix(p, "TZID="):
			var e error
			loc, e = calendarZone(strings.Trim(strings.TrimPrefix(p, "TZID="), "\""))
			if e != nil {
				return nil, e
			}
		default:
			return nil, fmt.Errorf("unsupported calendar date parameter %q", p)
		}
	}
	var out []time.Time
	for _, s := range strings.Split(value, ",") {
		layout := "20060102T150405"
		l := loc
		if dateOnly {
			layout = "20060102"
		} else if strings.HasSuffix(s, "Z") {
			layout = "20060102T150405Z"
			l = time.UTC
		}
		dt, e := time.ParseInLocation(layout, s, l)
		if e != nil {
			return nil, fmt.Errorf("invalid calendar recurrence date: %w", e)
		}
		if dt.Format(layout) != s {
			return nil, fmt.Errorf("unsupported calendar date in a DST gap")
		}
		if err := calendarCivilTime(dt); err != nil {
			return nil, err
		}
		out = append(out, dt)
	}
	return out, nil
}

// Reject invalid combinations that upstream accepts by normalization or overwrite.
func validateCalendarRule(value string) error {
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || seen[k] || k == "DTSTART" || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid or unsupported RRULE field %q", k)
		}
		seen[k] = true
		if (k == "COUNT" || k == "INTERVAL") && v == "0" {
			return fmt.Errorf("invalid RRULE %s=0", k)
		}
	}
	if seen["COUNT"] && seen["UNTIL"] {
		return fmt.Errorf("RRULE cannot combine COUNT and UNTIL")
	}
	return nil
}

// Ambiguous wall times require a fold policy that Thunderbird does not store.
func calendarCivilTime(t time.Time) error {
	for _, shift := range []time.Duration{-2 * time.Hour, -time.Hour, -30 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour} {
		other := t.Add(shift)
		if other.Year() == t.Year() && other.YearDay() == t.YearDay() && other.Hour() == t.Hour() && other.Minute() == t.Minute() {
			return fmt.Errorf("unsupported ambiguous calendar wall time")
		}
	}
	return nil
}
