# Configuration

## Environment variables

| Variable | Default | Effect |
| --- | --- | --- |
| `THUNDERBIRD_PROFILE` | auto-discovered | Path to the Thunderbird profile directory. |
| `THUNDERBIRD_READ_ONLY` | `false` | When `true`, mutating and sending actions are removed. |
| `THUNDERBIRD_ALLOW_SEND` | `false` | When `true`, the compose/send actions are enabled. |
| `THUNDERBIRD_TOOLS` | all | Comma-separated allowlist of tool names (`thunderbird_mail`, `thunderbird_contacts`, `thunderbird_calendar`). |

## Profile discovery

If `THUNDERBIRD_PROFILE` is not set, the server reads `profiles.ini` from the first
platform location that exists:

- `~/.thunderbird`
- `~/.mozilla-thunderbird`
- `~/snap/thunderbird/common/.thunderbird`
- `~/.var/app/org.mozilla.Thunderbird/.thunderbird` (Flatpak)
- `~/Library/Thunderbird` (macOS)
- `%APPDATA%\Thunderbird` (Windows)

It then selects the default profile. In Docker, bind-mount the profile and set
`THUNDERBIRD_PROFILE` to the mount point.

## Running alongside Thunderbird

The server reads the profile while Thunderbird is open. Reads normally go
straight to the profile's SQLite files, which coexist fine with a running
Thunderbird; contention only shows up when Thunderbird holds an exclusive lock
or a long write transaction.

When that happens, the server transparently copies the locked database (and its
`-wal` write-ahead log, which holds everything not yet checkpointed) to a
temporary directory and reads the copy instead. A copy is reused for up to 60
seconds, and after that it is re-made only if the source file actually changed,
so an actively indexing Thunderbird does not cause a copy per request. Copies
live under the system temporary directory (`$TMPDIR`) and are removed when the
server exits.

If a database is locked and the copy cannot be written, most often because
`$TMPDIR` is full, the request fails with an error naming the cause rather than
returning incomplete results.

Earlier versions behaved differently, and in one case badly: searches over the
Thunderbird index failed loudly with `database is locked`, while **contacts and
calendar failed silently** and could return empty or incomplete results,
because they read the main database file while ignoring the write-ahead log. If
you relied on contact or calendar results collected while Thunderbird was
running, they may have been missing recent entries; it is worth re-running those
queries.

## Credentials

IMAP/SMTP passwords are decrypted from the profile's `key4.db` + `logins.json`
(the NSS store) in pure Go. If the profile is protected by a **master password**,
decryption is not possible; the server logs a warning at startup and continues
with the read-only search/contacts/calendar tools available (IMAP/SMTP actions
then fail with a clear error).

**OAuth2** accounts (common for Gmail/Outlook) are not supported for live
IMAP/SMTP; they still appear in index search.
