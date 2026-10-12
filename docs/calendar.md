# Calendar ranges and recurrence

The server reads both `calendar-data/local.sqlite` and `calendar-data/cache.sqlite`.
It merges versions by calendar UUID, event UID, and recurrence identity.
The newest `last_modified` wins; local storage wins ties.
The server resolves versions before filtering or expanding events.

## Range and limits

`thunderbird_calendar` keeps the `list_calendars` and `list_events` actions.
`list_events` defaults to now through 30 days later, with 50 results.

- The range is `[since, until)`. An RFC3339 `until` is exclusive.
- A date-only `since` means midnight UTC.
- A date-only `until` includes that date, ending at the next midnight UTC.
- Positive-duration events qualify when they overlap the range.
- Zero-duration events qualify when their start falls within the range.
- Empty, reversed, and ranges longer than 366 days return errors.
- Results sort globally before limiting. Limits above 1000 clamp to 1000.

Work limits apply to the entire request, not each series:

| Budget | Maximum |
| --- | ---: |
| Event and recurrence rows (`calendarMaxRows`) | 10,000 |
| Generated and selected candidates (`calendarMaxCandidates`) | 50,000 |
| Frequency periods and preparation steps (`calendarMaxWork`) | 100,000 |
| Bytes per recurrence property (`calendarMaxRuleBytes`) | 8192 |

Budget exhaustion returns an error, never a partial success.
Requests have a ten-second context deadline. SQLite queries and recurrence loops
observe cancellation. Existing snapshot copying and lock acquisition can delay
cancellation; copying does not spawn a detached worker.
Very old series can exceed a budget even when the requested range is short.

## Recurrence and timezones

The server combines DTSTART, multiple RRULEs, and RDATEs, then subtracts EXDATEs.
Detached exceptions replace their original recurrence identity, not their new start.
Moved-in exceptions appear; moved-out, cancelled, and offline-deleted instances suppress their original occurrence.
A deleted master suppresses the entire series.
Exceptions use their own title, times, and location.

Daily, weekly, monthly, and yearly rules support COUNT, UNTIL, BYDAY, and BYSETPOS.
Other standard BY filters use the pinned recurrence engine.
RDATE and EXDATE accept date lists, DATE values, UTC values, and IANA TZID parameters.
Folded iCalendar lines are unfolded before parsing.

IANA timezones preserve local recurrence times across daylight-saving changes.
The binary includes the Go timezone database.
Timed events preserve elapsed duration. All-day events preserve civil-day duration,
so a day across a daylight-saving transition can last 23 or 25 hours.
Floating times and missing timezone identifiers use UTC, independent of the host timezone.

## Explicit unsupported cases

The server returns errors for unknown properties or timezone identifiers.
It also rejects:

- Embedded VTIMEZONE definitions, EXRULE, and RDATE periods.
- Hourly, minutely, and secondly rules, including old SECONDLY series.
- BYEASTER, leap seconds, and multiple BYHOUR, BYMINUTE, or BYSECOND values.
- Negative counts, intervals outside 1–10,000, duplicate rule fields, and COUNT combined with UNTIL.
- Recurrence years outside 1–9999 and generated times in daylight-saving gaps or ambiguous folds.
- Invalid date syntax, negative event durations, and oversized recurrence properties.

The engine checks work before empty frequency periods, not only returned occurrences.
Sparse or impossible rules therefore stop with a budget error instead of blocking indefinitely.
See `internal/rrule/README.md` for the pinned source and instrumentation.

## Thunderbird storage provenance

Constants come from immutable Thunderbird revision
`fb14c8128a259442aef7e4c1bbaf07ac5cd1e83d`:

- [Storage helpers](https://github.com/mozilla/releases-comm-central/blob/fb14c8128a259442aef7e4c1bbaf07ac5cd1e83d/calendar/providers/storage/calStorageHelpers.sys.mjs):
  `EVENT_ALLDAY=8`, `HAS_RECURRENCE=16`, and `RECURRENCE_ID_ALLDAY=512`.
- [Change log interface](https://github.com/mozilla/releases-comm-central/blob/fb14c8128a259442aef7e4c1bbaf07ac5cd1e83d/calendar/base/public/calIChangeLog.idl):
  `OFFLINE_FLAG_DELETED_RECORD=4`.
- [Storage item model](https://github.com/mozilla/releases-comm-central/blob/fb14c8128a259442aef7e4c1bbaf07ac5cd1e83d/calendar/providers/storage/CalStorageItemModel.sys.mjs):
  recurrence properties use `cal_recurrence.icalString`; cancellation uses `ical_status`.

## Verify

```sh
go test -count=1 -timeout 60s -run '^TestCalendar(RecurrenceRepair|RangeContract|MCPRegression|ExpansionBounds)$' -v .
go test -race -count=1 -timeout 600s ./...
go vet ./...
CGO_ENABLED=0 go build -o /dev/null .
git diff --check
```

Fixtures contain synthetic data. `TestCalendarMCPRegression` uses an actual MCP
client/server transport, not a direct handler call.

Legacy `/mozilla.org/<dtstamp>/` timezone prefixes resolve to their IANA suffix.
The `Z` alias resolves to UTC, matching Thunderbird's
[timezone service](https://github.com/mozilla/releases-comm-central/blob/fb14c8128a259442aef7e4c1bbaf07ac5cd1e83d/calendar/base/src/CalTimezoneService.sys.mjs).
Foreign serialized VTIMEZONE values return an explicit unsupported error.
