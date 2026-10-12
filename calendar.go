package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type calendarSource struct {
	db    *dbSource
	local bool
}

type Calendar struct {
	sources []calendarSource
	names   map[string]string // cal_id -> display name
}

func OpenCalendar(profileDir string, cals []CalendarRef) (*Calendar, error) {
	c := &Calendar{names: map[string]string{}}
	for _, ref := range cals {
		c.names[ref.UUID] = ref.Name
	}
	for _, file := range []string{"local.sqlite", "cache.sqlite"} {
		p := filepath.Join(profileDir, "calendar-data", file)
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, errors.Join(fmt.Errorf("calendar %s: %w", p, err), c.Close())
		}
		src, err := openDBSource(p, "calendar "+file)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("calendar %s: %w", p, err), c.Close())
		}
		c.sources = append(c.sources, calendarSource{db: src, local: file == "local.sqlite"})
	}
	if len(c.sources) == 0 {
		return nil, nil
	}
	return c, nil
}

// openCalendarAt retains the single-file entry point used by existing callers.
func openCalendarAt(path string, names map[string]string) (*Calendar, error) {
	src, err := openDBSource(path, "calendar")
	if err != nil {
		return nil, err
	}
	return &Calendar{sources: []calendarSource{{db: src, local: true}}, names: names}, nil
}

func (c *Calendar) Close() error {
	if c == nil {
		return nil
	}
	var errs []error
	for _, src := range c.sources {
		errs = append(errs, src.db.Close())
	}
	return errors.Join(errs...)
}

type calendarIdentity struct {
	calID, uid string
	recurrence sql.NullInt64
	timezone   sql.NullString
}
type storedCalendarEvent struct {
	key            calendarIdentity
	event          CalendarEvent
	modified       int64
	local          bool
	flags          int64
	deleted        bool
	startTZ, endTZ string
	source         *dbSource
}

// Inspect columns rather than retrying failed queries: missing optional columns
// in old fixtures are supported, but storage and required-schema errors propagate.
func calendarColumns(src *dbSource, table string) (map[string]bool, error) {
	rows, err := src.Query("SELECT * FROM " + table + " LIMIT 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, col := range cols {
		out[col] = true
	}
	return out, rows.Err()
}

func (s calendarSource) events(ctx context.Context, budget *calendarBudget, names map[string]string) ([]storedCalendarEvent, error) {
	cols, err := calendarColumns(s.db, "cal_events")
	if err != nil {
		return nil, err
	}
	props, err := calendarColumns(s.db, "cal_properties")
	if err != nil {
		return nil, err
	}
	optional := func(name string) string {
		if cols[name] {
			return "e." + name
		}
		return "NULL"
	}
	join := ""
	for _, name := range []string{"recurrence_id", "recurrence_id_tz"} {
		if props[name] {
			join += " AND p." + name + " IS " + optional(name)
		}
	}
	// A scalar subquery prevents duplicate properties from duplicating events.
	q := `SELECT e.cal_id, e.id, e.title, e.event_start, e.event_end, ` + optional("last_modified") + "," + optional("recurrence_id") + "," + optional("recurrence_id_tz") + `,
 (SELECT p.value FROM cal_properties p WHERE p.item_id=e.id AND p.cal_id=e.cal_id AND p.key='LOCATION'` + join + ` ORDER BY p.value LIMIT 1),` + optional("event_start_tz") + "," + optional("event_end_tz") + "," + optional("flags") + "," + optional("offline_journal") + "," + optional("ical_status") + ` FROM cal_events e LIMIT 10001`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedCalendarEvent
	for rows.Next() {
		var v storedCalendarEvent
		var title, loc sql.NullString
		var start, end int64
		var modified, flags, offline sql.NullInt64
		var startTZ, endTZ, status sql.NullString
		if err := budget.row(ctx); err != nil {
			return nil, err
		}
		if err := rows.Scan(&v.key.calID, &v.key.uid, &title, &start, &end, &modified, &v.key.recurrence, &v.key.timezone, &loc, &startTZ, &endTZ, &flags, &offline, &status); err != nil {
			return nil, err
		}
		v.key, err = canonicalCalendarIdentity(v.key, flags.Int64&calendarRecurrenceIDAllDay != 0)
		if err != nil {
			return nil, err
		}
		name := names[v.key.calID]
		if name == "" {
			name = v.key.calID
		}
		v.event = CalendarEvent{ID: v.key.uid, Calendar: name, Title: title.String, Location: loc.String, Start: time.UnixMicro(start).UTC(), End: time.UnixMicro(end).UTC()}
		v.modified, v.local = modified.Int64, s.local
		v.flags, v.deleted = flags.Int64, offline.Int64 == calendarOfflineDeleted || status.String == "CANCELLED"
		v.startTZ, v.endTZ, v.source = startTZ.String, endTZ.String, s.db
		out = append(out, v)
	}
	return out, rows.Err()
}

// eventLess provides stable ordering even when timestamps and display names tie.
func eventLess(a, b storedCalendarEvent) bool {
	if !a.event.Start.Equal(b.event.Start) {
		return a.event.Start.Before(b.event.Start)
	}
	if a.key.calID != b.key.calID {
		return a.key.calID < b.key.calID
	}
	if a.key.uid != b.key.uid {
		return a.key.uid < b.key.uid
	}
	if a.key.recurrence.Valid != b.key.recurrence.Valid {
		return !a.key.recurrence.Valid
	}
	if a.key.recurrence.Int64 != b.key.recurrence.Int64 {
		return a.key.recurrence.Int64 < b.key.recurrence.Int64
	}
	if a.key.timezone.Valid != b.key.timezone.Valid {
		return !a.key.timezone.Valid
	}
	if a.key.timezone.String != b.key.timezone.String {
		return a.key.timezone.String < b.key.timezone.String
	}
	if !a.event.End.Equal(b.event.End) {
		return a.event.End.Before(b.event.End)
	}
	if a.event.Title != b.event.Title {
		return a.event.Title < b.event.Title
	}
	return a.event.Location < b.event.Location
}

func (c *Calendar) Events(since, until time.Time, limit int) ([]CalendarEvent, error) {
	return c.EventsContext(context.Background(), since, until, limit)
}

func (c *Calendar) EventsContext(ctx context.Context, since, until time.Time, limit int) ([]CalendarEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !until.After(since) || until.Sub(since) > 366*24*time.Hour {
		return nil, fmt.Errorf("calendar range must be positive and at most 366 days")
	}
	if limit > 1000 {
		limit = 1000
	}
	if c == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	budget := &calendarBudget{}
	if limit <= 0 {
		limit = 50
	}
	merged := map[calendarIdentity]storedCalendarEvent{}
	for _, src := range c.sources {
		events, err := src.events(ctx, budget, c.names)
		if err != nil {
			return nil, fmt.Errorf("calendar %s: %w", src.db.path, err)
		}
		for _, v := range events {
			old, exists := merged[v.key]
			if !exists || v.modified > old.modified || (v.modified == old.modified && ((v.local && !old.local) || (v.local == old.local && eventLess(v, old)))) {
				merged[v.key] = v
			}
		}
	}
	selected, err := expandCalendar(ctx, budget, merged, since, until)
	if err != nil {
		return nil, err
	}
	sort.Slice(selected, func(i, j int) bool { return eventLess(selected[i], selected[j]) })
	if len(selected) > limit {
		selected = selected[:limit]
	}
	var out []CalendarEvent
	for _, v := range selected {
		out = append(out, v.event)
	}
	return out, nil
}
