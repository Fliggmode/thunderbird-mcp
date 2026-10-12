package main

import (
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
	key      calendarIdentity
	event    CalendarEvent
	modified int64
	local    bool
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

func (s calendarSource) events(names map[string]string) ([]storedCalendarEvent, error) {
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
 (SELECT p.value FROM cal_properties p WHERE p.item_id=e.id AND p.cal_id=e.cal_id AND p.key='LOCATION'` + join + ` ORDER BY p.value LIMIT 1) FROM cal_events e`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedCalendarEvent
	for rows.Next() {
		var v storedCalendarEvent
		var title, loc sql.NullString
		var start, end int64
		var modified sql.NullInt64
		if err := rows.Scan(&v.key.calID, &v.key.uid, &title, &start, &end, &modified, &v.key.recurrence, &v.key.timezone, &loc); err != nil {
			return nil, err
		}
		// Master rows have no recurrence identity, regardless of an unused TZ value.
		if !v.key.recurrence.Valid {
			v.key.timezone = sql.NullString{}
		}
		name := names[v.key.calID]
		if name == "" {
			name = v.key.calID
		}
		v.event = CalendarEvent{ID: v.key.uid, Calendar: name, Title: title.String, Location: loc.String, Start: time.UnixMicro(start).UTC(), End: time.UnixMicro(end).UTC()}
		v.modified, v.local = modified.Int64, s.local
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
	if c == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	merged := map[calendarIdentity]storedCalendarEvent{}
	for _, src := range c.sources {
		events, err := src.events(c.names)
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
	// Resolve versions before filtering: an obsolete in-range copy must not
	// reappear when the winning copy moved outside the requested range.
	var selected []storedCalendarEvent
	for _, v := range merged {
		if !v.event.Start.Before(since) && !v.event.Start.After(until) {
			selected = append(selected, v)
		}
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
