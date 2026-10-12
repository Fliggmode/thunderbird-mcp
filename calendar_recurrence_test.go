package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"testing"
	"time"
)

func repairTime(s string) time.Time {
	t, e := time.Parse(time.RFC3339, s)
	if e != nil {
		panic(e)
	}
	return t
}
func repairFixture(t *testing.T) (*Calendar, *sql.DB) {
	p := t.TempDir()
	db := calendarStorageDB(t, p, "cache.sqlite")
	storageExec(t, db, `ALTER TABLE cal_events ADD event_start_tz TEXT; ALTER TABLE cal_events ADD event_end_tz TEXT; ALTER TABLE cal_events ADD flags INTEGER; ALTER TABLE cal_events ADD offline_journal INTEGER; ALTER TABLE cal_events ADD ical_status TEXT; CREATE TABLE cal_recurrence(cal_id TEXT,item_id TEXT,icalString TEXT);`)
	c, e := OpenCalendar(p, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	return c, db
}
func repairEvent(t *testing.T, db *sql.DB, id, start, end, tz string, flags int, rid any) {
	storageExec(t, db, `INSERT INTO cal_events(cal_id,id,title,event_start,event_end,event_start_tz,event_end_tz,flags,recurrence_id) VALUES('c',?,?,?,?,?,?,?,?)`, id, id, repairTime(start).UnixMicro(), repairTime(end).UnixMicro(), tz, tz, flags, rid)
}
func repairRule(t *testing.T, db *sql.DB, id, rule string) {
	storageExec(t, db, `INSERT INTO cal_recurrence VALUES('c',?,?)`, id, rule)
}
func TestCalendarRecurrenceRepair(t *testing.T) {
	t.Run("additional", calendarRecurrenceAdditional)
	t.Run("union exceptions location", func(t *testing.T) {
		c, db := repairFixture(t)
		repairEvent(t, db, "r", "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z", "UTC", 16, nil)
		for _, r := range []string{"RRULE:FREQ=DAILY;COUNT=4", "RRULE:FREQ=WEEKLY;COUNT=2", "RDATE:20260309T100000Z", "EXDATE:20260302T100000Z"} {
			repairRule(t, db, "r", r)
		}
		repairEvent(t, db, "r", "2026-03-06T12:00:00Z", "2026-03-06T13:00:00Z", "UTC", 0, repairTime("2026-03-03T10:00:00Z").UnixMicro())
		storageExec(t, db, `INSERT INTO cal_properties VALUES('c','r','LOCATION','Moved',?,NULL)`, repairTime("2026-03-03T10:00:00Z").UnixMicro())
		ev, e := c.Events(repairTime("2026-03-02T00:00:00Z"), repairTime("2026-03-10T00:00:00Z"), 50)
		if e != nil || len(ev) != 4 {
			t.Fatalf("events=%+v err=%v", ev, e)
		}
		if ev[1].Location != "Moved" {
			t.Fatalf("exception location: %+v", ev)
		}
	})
	t.Run("monthly setpos", func(t *testing.T) {
		c, db := repairFixture(t)
		repairEvent(t, db, "r", "2026-01-30T09:00:00Z", "2026-01-30T10:00:00Z", "UTC", 16, nil)
		repairRule(t, db, "r", "RRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1;UNTIL=20260331T090000Z")
		ev, e := c.Events(repairTime("2026-02-01T00:00:00Z"), repairTime("2026-04-01T00:00:00Z"), 50)
		if e != nil || len(ev) != 2 || ev[0].Start.Day() != 27 {
			t.Fatalf("%+v %v", ev, e)
		}
	})
	t.Run("DST", func(t *testing.T) {
		c, db := repairFixture(t)
		repairEvent(t, db, "r", "2026-03-07T14:00:00Z", "2026-03-07T15:00:00Z", "America/New_York", 16, nil)
		repairRule(t, db, "r", "RRULE:FREQ=DAILY;COUNT=3")
		ev, e := c.Events(repairTime("2026-03-08T00:00:00Z"), repairTime("2026-03-10T00:00:00Z"), 50)
		if e != nil || len(ev) != 2 || ev[0].Start.UTC().Hour() != 13 {
			t.Fatalf("%+v %v", ev, e)
		}
	})
}
func TestCalendarRangeContract(t *testing.T) {
	c, db := repairFixture(t)
	repairEvent(t, db, "overlap", "2026-03-01T23:00:00Z", "2026-03-02T01:00:00Z", "UTC", 0, nil)
	repairEvent(t, db, "end", "2026-03-03T00:00:00Z", "2026-03-03T00:00:00Z", "UTC", 0, nil)
	a, b := repairTime("2026-03-02T00:00:00Z"), repairTime("2026-03-03T00:00:00Z")
	ev, e := c.Events(a, b, 50)
	if e != nil || len(ev) != 1 || ev[0].ID != "overlap" {
		t.Errorf("%+v %v", ev, e)
	}
	for _, end := range []time.Time{a, a.Add(-time.Hour), a.AddDate(0, 0, 367)} {
		if _, e := c.Events(a, end, 50); e == nil {
			t.Error("invalid range accepted")
		}
	}
}
func TestCalendarExpansionBounds(t *testing.T) {
	t.Run("additional", calendarBoundsAdditional)
	for _, rule := range []string{"RRULE:FREQ=SECONDLY", "RRULE:FREQ=DAILY;BYMONTH=2;BYMONTHDAY=30", "EXRULE:FREQ=DAILY", "RDATE;VALUE=PERIOD:20260301T000000Z/PT1H"} {
		t.Run(rule, func(t *testing.T) {
			c, db := repairFixture(t)
			repairEvent(t, db, "r", "1900-01-01T00:00:00Z", "1900-01-01T01:00:00Z", "UTC", 16, nil)
			repairRule(t, db, "r", rule)
			_, e := c.Events(repairTime("2026-03-01T00:00:00Z"), repairTime("2026-03-02T00:00:00Z"), 50)
			if e == nil {
				t.Fatal("expected explicit unsupported/budget error")
			}
		})
	}
}
func TestCalendarMCPRegression(t *testing.T) {
	c, db := repairFixture(t)
	repairEvent(t, db, "r", "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z", "UTC", 16, nil)
	repairRule(t, db, "r", "RRULE:FREQ=DAILY;COUNT=3")
	old := app
	app = &App{Calendar: c}
	t.Cleanup(func() { app = old })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "thunderbird_calendar", InputSchema: mustSchema[CalendarInput]([]any{"list_calendars", "list_events"})}, ThunderbirdCalendar)
	ct, st := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, st, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, e := client.Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	res, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "thunderbird_calendar", Arguments: map[string]any{"action": "list_events", "since": "2026-03-02", "until": "2026-03-02"}})
	if e != nil {
		t.Fatal(e)
	}
	bad, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "thunderbird_calendar", Arguments: map[string]any{"action": "list_events", "since": "2026-03-03T00:00:00Z", "until": "2026-03-02T00:00:00Z"}})
	if e != nil || !bad.IsError {
		t.Fatalf("invalid range MCP result=%+v err=%v", bad, e)
	}
	raw, _ := json.Marshal(res)
	if res.IsError || !strings.Contains(string(raw), "2026-03-02T10:00:00Z") {
		t.Fatalf("%s", raw)
	}
}

func calendarRecurrenceAdditional(t *testing.T) {
	t.Run("Thunderbird timezone encodings", func(t *testing.T) {
		for _, tz := range []string{"/mozilla.org/20050126_1/America/New_York", "BEGIN:VTIMEZONE\r\nTZID:Custom\r\nEND:VTIMEZONE", "Unknown/Zone"} {
			c, db := repairFixture(t)
			repairEvent(t, db, "r", "2026-03-07T14:00:00Z", "2026-03-07T15:00:00Z", tz, 16, nil)
			repairRule(t, db, "r", "RRULE:FREQ=DAILY;COUNT=3")
			ev, e := c.Events(repairTime("2026-03-08T00:00:00Z"), repairTime("2026-03-10T00:00:00Z"), 50)
			if strings.HasPrefix(tz, "/mozilla.org/") {
				if e != nil || len(ev) != 2 || ev[0].Start.UTC().Hour() != 13 {
					t.Fatalf("%+v %v", ev, e)
				}
			} else if e == nil {
				t.Fatal("expected unsupported timezone error")
			}
		}
	})

	t.Run("DST gap and fold errors", func(t *testing.T) {
		for _, start := range []string{"2026-03-07T07:30:00Z", "2026-10-31T05:30:00Z"} {
			c, db := repairFixture(t)
			a := repairTime(start)
			repairEvent(t, db, "r", start, a.Add(time.Hour).Format(time.RFC3339), "America/New_York", 16, nil)
			repairRule(t, db, "r", "RRULE:FREQ=DAILY;COUNT=3")
			if _, e := c.Events(a, a.AddDate(0, 0, 3), 50); e == nil {
				t.Fatal("expected DST ambiguity/gap error")
			}
		}
	})

	t.Run("all day civil duration and floating", func(t *testing.T) {
		c, db := repairFixture(t)
		repairEvent(t, db, "all", "2026-03-07T05:00:00Z", "2026-03-08T05:00:00Z", "America/New_York", 24, nil)
		repairRule(t, db, "all", "RRULE:FREQ=DAILY;COUNT=3")
		repairEvent(t, db, "float", "2026-03-07T09:00:00Z", "2026-03-07T10:00:00Z", "floating", 16, nil)
		repairRule(t, db, "float", "RRULE:FREQ=DAILY;COUNT=3")
		ev, e := c.Events(repairTime("2026-03-08T00:00:00Z"), repairTime("2026-03-10T00:00:00Z"), 50)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, v := range ev {
			if v.ID == "all" && v.Start.Day() == 8 {
				found = true
				if v.End.Sub(v.Start) != 23*time.Hour {
					t.Fatalf("%+v", v)
				}
			}
			if v.ID == "float" && v.Start.Hour() != 9 {
				t.Fatalf("%+v", v)
			}
		}
		if !found {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("cancelled offline moved out and moved in", func(t *testing.T) {
		c, db := repairFixture(t)
		repairEvent(t, db, "r", "2026-03-01T10:00:00Z", "2026-03-01T11:00:00Z", "UTC", 16, nil)
		repairRule(t, db, "r", "RRULE:FREQ=DAILY;COUNT=6")
		for _, day := range []string{"02", "03", "04", "06"} {
			start := "2026-03-" + day + "T10:00:00Z"
			end := "2026-03-" + day + "T11:00:00Z"
			repairEvent(t, db, "r", start, end, "UTC", 0, repairTime(start).UnixMicro())
		}
		storageExec(t, db, `UPDATE cal_events SET ical_status='CANCELLED' WHERE recurrence_id=?`, repairTime("2026-03-02T10:00:00Z").UnixMicro())
		storageExec(t, db, `UPDATE cal_events SET offline_journal=4 WHERE recurrence_id=?`, repairTime("2026-03-03T10:00:00Z").UnixMicro())
		storageExec(t, db, `UPDATE cal_events SET event_start=?,event_end=? WHERE recurrence_id=?`, repairTime("2026-04-04T10:00:00Z").UnixMicro(), repairTime("2026-04-04T11:00:00Z").UnixMicro(), repairTime("2026-03-04T10:00:00Z").UnixMicro())
		storageExec(t, db, `UPDATE cal_events SET event_start=?,event_end=? WHERE recurrence_id=?`, repairTime("2026-03-02T12:00:00Z").UnixMicro(), repairTime("2026-03-02T13:00:00Z").UnixMicro(), repairTime("2026-03-06T10:00:00Z").UnixMicro())
		ev, e := c.Events(repairTime("2026-03-02T00:00:00Z"), repairTime("2026-03-05T00:00:00Z"), 50)
		if e != nil || len(ev) != 1 || ev[0].Start.Hour() != 12 {
			t.Fatalf("%+v %v", ev, e)
		}
	})
	t.Run("yearly and folded date lists", func(t *testing.T) {
		c, db := repairFixture(t)
		repairEvent(t, db, "r", "2020-02-29T09:00:00Z", "2020-02-29T10:00:00Z", "UTC", 16, nil)
		repairRule(t, db, "r", "RRULE:FREQ=YEARLY;COUNT=3")
		repairRule(t, db, "r", "RDATE:20280228T090000Z,\r\n 20280301T090000Z")
		repairRule(t, db, "r", "EXDATE:20280228T090000Z")
		ev, e := c.Events(repairTime("2028-02-01T00:00:00Z"), repairTime("2028-04-01T00:00:00Z"), 50)
		if e != nil || len(ev) != 2 || ev[0].Start.Day() != 29 {
			t.Fatalf("%+v %v", ev, e)
		}
	})
}

func calendarBoundsAdditional(t *testing.T) {
	t.Run("cancel context", func(t *testing.T) {
		c, _ := repairFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, e := c.EventsContext(ctx, time.Now(), time.Now().Add(time.Hour), 50); e != context.Canceled {
			t.Fatalf("%v", e)
		}
	})
	t.Run("row cap", func(t *testing.T) {
		c, db := repairFixture(t)
		storageExec(t, db, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10001) INSERT INTO cal_events(cal_id,id,event_start,event_end) SELECT 'c',x,0,1 FROM n`)
		_, e := c.Events(time.Unix(0, 0), time.Unix(1, 0), 50)
		if e == nil || !strings.Contains(e.Error(), "row budget") {
			t.Fatalf("%v", e)
		}
	})
	t.Run("candidate cap", func(t *testing.T) {
		c, db := repairFixture(t)
		repairEvent(t, db, "r", "1800-01-01T00:00:00Z", "1800-01-01T01:00:00Z", "UTC", 16, nil)
		repairRule(t, db, "r", "RRULE:FREQ=DAILY")
		_, e := c.Events(repairTime("2026-01-01T00:00:00Z"), repairTime("2026-01-02T00:00:00Z"), 50)
		if e == nil || !strings.Contains(e.Error(), "candidate budget") {
			t.Fatalf("%v", e)
		}
	})
	t.Run("result cap defaults and deterministic prefix", func(t *testing.T) {
		c, db := repairFixture(t)
		storageExec(t, db, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<1002) INSERT INTO cal_events(cal_id,id,event_start,event_end) SELECT 'c',printf('%04d',x),x,x+1 FROM n`)
		for limit, want := range map[int]int{0: 50, 1: 1, 1001: 1000} {
			ev, e := c.Events(time.UnixMicro(0), time.UnixMicro(2000), limit)
			if e != nil || len(ev) != want || ev[0].ID != "0001" {
				t.Fatalf("len=%d %v", len(ev), e)
			}
		}
	})
	for _, rule := range []string{"RRULE:FREQ=DAILY;COUNT=0", "RRULE:FREQ=DAILY;COUNT=2;UNTIL=20260303T000000Z", "RRULE:FREQ=DAILY;BYHOUR=8,9", "RRULE:FREQ=DAILY;X-UNKNOWN=1", "RRULE:FREQ=DAILY;FREQ=WEEKLY"} {
		t.Run(rule, func(t *testing.T) {
			c, db := repairFixture(t)
			repairEvent(t, db, "r", "2026-03-01T00:00:00Z", "2026-03-01T01:00:00Z", "UTC", 16, nil)
			repairRule(t, db, "r", rule)
			if _, e := c.Events(repairTime("2026-03-01T00:00:00Z"), repairTime("2026-03-04T00:00:00Z"), 50); e == nil {
				t.Fatal("expected error")
			}
		})
	}
}
