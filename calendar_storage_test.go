package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Relevant Thunderbird storage columns, including nullable master recurrence IDs.
const calendarStorageSchema = `CREATE TABLE cal_events (
 cal_id TEXT, id TEXT, title TEXT, event_start INTEGER, event_end INTEGER,
 last_modified INTEGER, recurrence_id INTEGER, recurrence_id_tz TEXT);
 CREATE TABLE cal_properties (cal_id TEXT, item_id TEXT, key TEXT, value BLOB,
 recurrence_id INTEGER, recurrence_id_tz TEXT);`

func calendarStorageDB(t *testing.T, profile, file string) *sql.DB {
	t.Helper()
	dir := filepath.Join(profile, "calendar-data")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(calendarStorageSchema); err != nil {
		t.Fatal(err)
	}
	return db
}
func storageExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func storageEvent(t *testing.T, db *sql.DB, cal, id, title string, start, modified int64, rid any) {
	t.Helper()
	storageExec(t, db, `INSERT INTO cal_events VALUES(?,?,?,?,?,?,?,NULL)`, cal, id, title, start, start+10, modified, rid)
}
func storageEvents(t *testing.T, c *Calendar, limit int) []CalendarEvent {
	t.Helper()
	ev, err := c.Events(time.UnixMicro(0), time.UnixMicro(1000), limit)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}
func TestCalendarStorageRepair(t *testing.T) {
	for _, file := range []string{"local.sqlite", "cache.sqlite"} {
		t.Run(file, func(t *testing.T) {
			p := t.TempDir()
			db := calendarStorageDB(t, p, file)
			storageEvent(t, db, "uuid", "one", "only", 100, 1, nil)
			c, err := OpenCalendar(p, []CalendarRef{{UUID: "uuid", Name: "Shared name"}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ev := storageEvents(t, c, 10)
			if len(ev) != 1 || ev[0].Calendar != "Shared name" {
				t.Fatalf("events = %+v", ev)
			}
		})
	}
	t.Run("merge identity versions and global order", func(t *testing.T) {
		p := t.TempDir()
		local := calendarStorageDB(t, p, "local.sqlite")
		cache := calendarStorageDB(t, p, "cache.sqlite")
		storageEvent(t, local, "a", "uid", "old", 100, 1, nil)
		storageEvent(t, cache, "a", "uid", "new", 200, 2, nil)
		storageEvent(t, local, "a", "tie", "local tie", 200, 3, nil)
		storageEvent(t, cache, "a", "tie", "cache tie", 200, 3, nil)
		storageEvent(t, local, "b", "uid", "other calendar", 200, 1, nil)
		storageEvent(t, cache, "a", "uid", "exception", 200, 2, 150)
		storageEvent(t, cache, "a", "uid", "second exception", 200, 2, 160)
		storageEvent(t, local, "a", "moved", "stale in range", 100, 1, nil)
		storageEvent(t, cache, "a", "moved", "new outside range", 2000, 2, nil)
		storageExec(t, cache, `INSERT INTO cal_properties VALUES ('a','uid','LOCATION','master',NULL,NULL),('a','uid','LOCATION','exception place',150,NULL)`)
		c, err := OpenCalendar(p, []CalendarRef{{UUID: "a", Name: "Same"}, {UUID: "b", Name: "Same"}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		want := []string{"local tie", "new", "exception", "second exception", "other calendar"}
		for i := 0; i < 5; i++ {
			ev := storageEvents(t, c, 50)
			var titles []string
			for _, e := range ev {
				titles = append(titles, e.Title)
			}
			if !reflect.DeepEqual(titles, want) {
				t.Fatalf("titles = %v, want %v", titles, want)
			}
			if ev[1].Location != "master" || ev[2].Location != "exception place" || ev[3].Location != "" {
				t.Fatalf("locations = %+v", ev)
			}
			if got := storageEvents(t, c, 2); !reflect.DeepEqual(got, ev[:2]) {
				t.Fatalf("global limit = %+v", got)
			}
		}
	})
	t.Run("absent", func(t *testing.T) {
		c, err := OpenCalendar(t.TempDir(), nil)
		if err != nil || c != nil {
			t.Fatalf("%v %v", c, err)
		}
	})
	for _, kind := range []string{"corrupt local", "corrupt cache", "stat error", "schema error"} {
		t.Run(kind, func(t *testing.T) {
			p := t.TempDir()
			dir := filepath.Join(p, "calendar-data")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "stat error":
				if err := os.Symlink("local.sqlite", filepath.Join(dir, "local.sqlite")); err != nil {
					t.Fatal(err)
				}
			case "schema error":
				db, err := sql.Open("sqlite", filepath.Join(dir, "local.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				storageExec(t, db, "CREATE TABLE unrelated(x)")
				db.Close()
			default:
				file := "local.sqlite"
				if kind == "corrupt cache" {
					file = "cache.sqlite"
					db := calendarStorageDB(t, p, "local.sqlite")
					storageEvent(t, db, "a", "ok", "ok", 10, 1, nil)
				}
				if err := os.WriteFile(filepath.Join(dir, file), []byte("not a database"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := OpenCalendar(p, nil)
			if err == nil && c != nil {
				_, err = c.Events(time.UnixMicro(0), time.UnixMicro(1000), 10)
			}
			c.Close()
			if err == nil {
				t.Fatal("storage error swallowed")
			}
			a, err := loadAppFromConfig(&Config{ProfileDir: p}, &Credentials{}, true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			old := app
			app = a
			defer func() { app = old }()
			_, _, err = ListEvents(context.Background(), nil, ListEventsInput{})
			if err == nil || strings.Contains(err.Error(), "no calendar available") {
				t.Fatalf("storage cause lost: %v", err)
			}
		})
	}
	t.Run("WAL both sources and close", func(t *testing.T) {
		p := t.TempDir()
		for _, file := range []string{"local.sqlite", "cache.sqlite"} {
			db := calendarStorageDB(t, p, file)
			storageExec(t, db, "PRAGMA journal_mode=WAL")
			storageExec(t, db, "PRAGMA wal_autocheckpoint=0")
			storageEvent(t, db, "a", file, file, 100, 1, nil)
		}
		c, err := OpenCalendar(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if ev := storageEvents(t, c, 10); len(ev) != 2 {
			t.Fatalf("WAL events = %+v", ev)
		}
		var snapshotDirs []string
		for _, src := range c.sources {
			if err := src.db.makeSnapshotNow(); err != nil {
				t.Fatal(err)
			}
			snapshotDirs = append(snapshotDirs, src.db.snapDir)
		}
		if ev := storageEvents(t, c, 10); len(ev) != 2 {
			t.Fatalf("snapshot WAL events = %+v", ev)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		for _, src := range c.sources {
			if err := src.db.live.Ping(); err == nil {
				t.Fatal("live source still open")
			}
			if err := src.db.snap.Ping(); err == nil {
				t.Fatal("snapshot still open")
			}
		}
		for _, dir := range snapshotDirs {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("snapshot not removed: %s: %v", dir, err)
			}
		}
		if _, err := c.Events(time.UnixMicro(0), time.UnixMicro(1000), 10); err == nil {
			t.Fatal("read after close succeeded")
		}
	})
}
