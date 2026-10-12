package main

import (
	"database/sql"
	"testing"
	"time"
)

func TestCalendarEquivalentRecurrenceIdentities(t *testing.T) {
	for _, zones := range []struct{ name, old, new string }{
		{"UTC alias", "UTC", "Z"},
		{"legacy IANA", "/mozilla.org/20050126_1/America/New_York", "America/New_York"},
		{"zoned instant", "America/New_York", "UTC"},
		{"floating", "floating", "floating"},
		{"missing zone", "", "UTC"},
	} {
		for _, action := range []string{"modified", "cancelled", "moved out", "moved in", "local tie"} {
			t.Run(zones.name+"/"+action, func(t *testing.T) {
				c, cache := repairFixture(t)
				p := t.TempDir()
				local := calendarStorageDB(t, p, "local.sqlite")
				storageExec(t, local, `ALTER TABLE cal_events ADD event_start_tz TEXT; ALTER TABLE cal_events ADD event_end_tz TEXT; ALTER TABLE cal_events ADD flags INTEGER; ALTER TABLE cal_events ADD offline_journal INTEGER; ALTER TABLE cal_events ADD ical_status TEXT; CREATE TABLE cal_recurrence(cal_id TEXT,item_id TEXT,icalString TEXT);`)
				lc, err := OpenCalendar(p, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer lc.Close()
				c.sources = append(c.sources, lc.sources...)
				repairEvent(t, cache, "r", "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z", zones.new, 16, nil)
				repairRule(t, cache, "r", "RRULE:FREQ=DAILY;COUNT=3")
				rid := repairTime("2026-03-02T10:00:00Z").UnixMicro()
				oldStart, newStart := "2026-03-02T12:00:00Z", "2026-03-02T14:00:00Z"
				if action == "moved out" {
					newStart = "2026-03-04T14:00:00Z"
				}
				if action == "moved in" {
					oldStart = "2026-03-04T12:00:00Z"
				}
				for _, v := range []struct {
					db               *sql.DB
					start, tz, title string
					modified         int
				}{
					{cache, oldStart, zones.old, "stale", 1}, {local, newStart, zones.new, "winner", 2},
				} {
					repairEvent(t, v.db, "r", v.start, repairTime(v.start).Add(time.Hour).Format(time.RFC3339), zones.new, 0, rid)
					storageExec(t, v.db, `UPDATE cal_events SET recurrence_id_tz=?,last_modified=?,title=? WHERE recurrence_id IS NOT NULL`, v.tz, v.modified, v.title)
				}
				if action == "cancelled" {
					storageExec(t, local, `UPDATE cal_events SET ical_status='CANCELLED'`)
				}
				if action == "local tie" {
					storageExec(t, cache, `UPDATE cal_events SET last_modified=2`)
				}
				a := repairTime("2026-03-02T00:00:00Z")
				for i := 0; i < 5; i++ {
					ev, err := c.Events(a, a.Add(24*time.Hour), 50)
					want := 1
					if action == "cancelled" || action == "moved out" {
						want = 0
					}
					if err != nil || len(ev) != want {
						t.Fatalf("events=%+v err=%v want count=%d", ev, err, want)
					}
					if want == 1 && (ev[0].Title != "winner" || !ev[0].Start.Equal(repairTime(newStart))) {
						t.Fatalf("wrong version: %+v", ev)
					}
				}
			})
		}
	}
}

func TestCalendarRecurrenceIdentityKinds(t *testing.T) {
	for _, tc := range []struct {
		name, zone string
		flags      int
		rid        string
		ridZone    string
		ridFlags   int
		count      int
	}{
		{"floating overlay", "floating", 16, "2026-03-02T10:00:00Z", "floating", 0, 0},
		{"floating is not instant", "floating", 16, "2026-03-02T10:00:00Z", "UTC", 0, 1},
		{"date overlay across DST", "America/New_York", 24, "2026-03-09T04:00:00Z", "/mozilla.org/20050126_1/America/New_York", 512, 0},
		{"date is not midnight instant", "America/New_York", 24, "2026-03-09T04:00:00Z", "America/New_York", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, db := repairFixture(t)
			start, end, since := "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z", "2026-03-02T00:00:00Z"
			if tc.flags&8 != 0 {
				start, end, since = "2026-03-07T05:00:00Z", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z"
			}
			repairEvent(t, db, "r", start, end, tc.zone, tc.flags, nil)
			repairRule(t, db, "r", "RRULE:FREQ=DAILY;COUNT=3")
			repairEvent(t, db, "r", tc.rid, tc.rid, tc.zone, tc.ridFlags, repairTime(tc.rid).UnixMicro())
			storageExec(t, db, `UPDATE cal_events SET recurrence_id_tz=?,ical_status='CANCELLED' WHERE recurrence_id IS NOT NULL`, tc.ridZone)
			a := repairTime(since)
			ev, err := c.Events(a, a.Add(24*time.Hour), 50)
			if err != nil || len(ev) != tc.count {
				t.Fatalf("events=%+v err=%v want count=%d", ev, err, tc.count)
			}
		})
	}
	// DATE keys preserve the civil date even when the stored instant differs.
	a := calendarIdentity{recurrence: sql.NullInt64{Int64: repairTime("2026-03-09T04:00:00Z").UnixMicro(), Valid: true}, timezone: sql.NullString{String: "America/New_York", Valid: true}}
	b := a
	b.recurrence.Int64 = repairTime("2026-03-09T00:00:00Z").UnixMicro()
	b.timezone.String = "UTC"
	a, err := canonicalCalendarIdentity(a, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err = canonicalCalendarIdentity(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("same civil date has different identity: %+v %+v", a, b)
	}
}
