# Reading a live Thunderbird profile — design

Date: 2026-07-21
Status: approved

## Problem

When Thunderbird is running, `search_messages` / `list_recent` fail with
`database is locked`. The three SQLite sources currently disagree about how to
open a profile file:

| File | DSN | Behavior with Thunderbird running |
|---|---|---|
| `gloda.go:28` | `mode=ro&_pragma=busy_timeout(5000)` | takes real locks → returns `SQLITE_BUSY` |
| `contacts.go:22` | `mode=ro&immutable=1` | ignores locking → never errors |
| `calendar.go:30` | `mode=ro&immutable=1` | ignores locking → never errors |

Gloda is the only one that respects locking, which is why it is the one that
failed. The other two are not correct — they are silently unsafe.
`immutable=1` tells SQLite the file never changes, so it **ignores the `-wal`
file entirely**: if the database is in WAL mode, those reads return a stale
pre-checkpoint snapshot with no error. The visible bug and this latent one have
the same root cause: there is no single, deliberate answer to "how do we read a
live profile".

A second-order issue: `LoadApp` opens all three databases once at startup and
holds the handles for the process lifetime (`main.go:47`, `app.go:48-62`).
`sql.Open` is lazy, so nothing fails at boot and the existing
`gloda unavailable: ...` log never fires for this class of failure — the error
only surfaces on the first query.

## Goal

Reads succeed whether or not Thunderbird is running, return current data, and
fail with an actionable message when they cannot.

## Design

### `dbSource` (new file `dbsource.go`)

One type owns one profile SQLite file. It is a drop-in replacement for the
`*sql.DB` field in `Gloda`, `Contacts`, and `Calendar` — all three use only
`Query` and `Close`, so the call sites change type, not shape.

```go
type dbSource struct {
    path, name string      // profile file; "gloda"|"contacts"|"calendar" for errors

    mu       sync.Mutex
    live     *sql.DB       // read-only handle on the profile file
    snap     *sql.DB       // handle on our private copy; nil until first contention
    snapDir  string
    snapAt   time.Time
    srcMod   time.Time
}
```

Exposed API is exactly `Query(q string, args ...any) (*sql.Rows, error)` and
`Close() error`.

### Read path

`Query` tries, in order:

1. **Warm snapshot.** If `snap != nil` and `time.Since(snapAt) < snapshotTTL`,
   query it. Contention is sticky for the TTL window; the live file is not
   re-probed on every call.
2. **Live.** `file:<path>?mode=ro&_pragma=busy_timeout(5000)`. This is the fast
   path when Thunderbird is closed, and costs nothing.
3. **Snapshot, on contention.** Only when the error is `SQLITE_BUSY` or
   `SQLITE_LOCKED`. Every other error — missing file, corrupt database, bad SQL
   — propagates unchanged. Real bugs must not be papered over with a copy.

`snapshotTTL` is a package constant of 60s. Refresh happens only when the TTL
has expired **and** the source mtime differs from `srcMod`, so an idle
Thunderbird costs zero copies no matter how long the session runs. The mtime
check matters because gloda's mtime churns constantly while the indexer runs;
without the TTL debounce that would mean re-copying a possibly large database on
nearly every tool call.

### Snapshotting

Copy `X.sqlite`, plus `X.sqlite-wal` and `X.sqlite-shm` when present, into a
per-process directory from `os.MkdirTemp`, removed on `Close`.

The copy is opened **read-write** (plain `file:<tmp>`), not `mode=ro`. This is
the point the whole design rests on: a read-only connection cannot run WAL
recovery, so it would ignore the copied `-wal` and serve exactly the stale
pre-checkpoint data that `immutable=1` serves today. Because the copy is our
private throwaway, SQLite replays the WAL and we see Thunderbird's
un-checkpointed writes.

The copy is not atomic and can therefore be torn. WAL replay heals the common
case. If a query against a freshly made snapshot fails with *malformed* or *not
a database*, re-copy once and retry; a second failure is returned to the caller.

### Call-site changes

- `gloda.go` — DSN moves into `dbSource`; `Gloda.db` becomes `*dbSource`.
- `contacts.go`, `calendar.go` — `immutable=1` is **deleted**. Both go through
  the same live→snapshot ladder and stop serving stale data.
- `app.go` — unchanged in shape. Open still returns `(nil, nil)` for an absent
  file. A startup probe (`SELECT 1`) makes the existing "unavailable" log
  truthful instead of lazily wrong; a `BUSY` at probe time is **not** fatal, it
  just means the first real query will snapshot.

### Errors

When snapshotting itself fails, the wrapped error names the cause and the fix
instead of surfacing `database is locked`:

```
gloda: Thunderbird is holding the search index and it could not be copied:
no space left on device (/tmp). Free space in $TMPDIR or close Thunderbird.
```

There is no `immutable=1` last-resort tier. Failing loudly beats quietly
returning wrong results.

## Testing

Contention is reproducible in-process, so these are real tests rather than
mocks: open a scratch database read-write, `PRAGMA locking_mode=EXCLUSIVE`,
begin a write transaction — a second connection then gets `BUSY`.

- falls back to a snapshot under a live exclusive-lock holder, and returns the
  correct rows
- **snapshot sees un-checkpointed WAL writes** — the regression test for the
  `immutable=1` bug; write in WAL mode without checkpointing and assert the row
  is visible
- a warm snapshot is reused inside the TTL (assert no second copy is made)
- non-`BUSY` errors propagate untouched and trigger no snapshot attempt
- an unwritable snapshot directory produces the actionable message, not
  `database is locked`

## Known limitation

`Query` retries on the initial call, but SQLite can surface `BUSY` later, during
`rows.Next()`, where it reaches the caller through `rows.Err()`. Once a snapshot
is warm every query in the TTL window uses it, so the exposure is at most one
query per window. Documented rather than fixed — closing it would mean buffering
every result set in memory.

## Rejected alternatives

- **`nolock=1` direct reads.** Free and zero-disk, but WAL readers need write
  access to the `-shm`, so `nolock` either fails or silently serves a stale
  pre-checkpoint snapshot — the same class of bug as the current `immutable=1`.
- **Always snapshot.** Simpler (one code path) but pays a full copy even when
  Thunderbird is closed, which is the case where everything already works.
- **Bypass the databases via IMAP/mbox.** `imap.go` and `mbox.go` already exist
  and live IMAP `SEARCH` never touches a lock, but it needs credentials (which
  can fail on a master password), it is slow, and there is no alternative source
  at all for contacts or calendar. Viable later as a *fallback* for mail; not
  the fix.
