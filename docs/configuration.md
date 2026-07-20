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

## Credentials

IMAP/SMTP passwords are decrypted from the profile's `key4.db` + `logins.json`
(the NSS store) in pure Go. If the profile is protected by a **master password**,
decryption is not possible; the server logs a warning at startup and continues
with the read-only search/contacts/calendar tools available (IMAP/SMTP actions
then fail with a clear error).

**OAuth2** accounts (common for Gmail/Outlook) are not supported for live
IMAP/SMTP; they still appear in index search.
