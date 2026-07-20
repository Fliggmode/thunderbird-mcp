# Security model

## Read-only guarantees

- The server **never writes** to any Thunderbird-owned file: mbox/maildir, the
  gloda index, `abook.sqlite`, the calendar database, `key4.db`, or `logins.json`.
- Every SQLite database is opened read-only (`mode=ro`, and `immutable=1` where a
  Thunderbird process might hold it).
- All message mutations go through **IMAP**, so Thunderbird picks up the change on
  its next sync rather than the server editing local state behind its back.

## Local folders are read-only

POP and "Local Folders" accounts store mail in on-disk mbox files. Writing to them
while Thunderbird is running risks corruption, so `set_flags`, `move_message`, and
`delete_message` reject local folders with a clear error. Reading them
(`search_messages`, `get_message`) is fully supported.

## Path-traversal protection

Message references are opaque `<folderURI>#<key>` strings. For local folders the
key is a byte offset and the folder URI maps to an on-disk file. The resolver
rejects any path segment that is `.`/`..`/empty or contains a separator, and
verifies the resolved path stays within the account's mail directory — so a
crafted `message_ref` cannot read arbitrary files.

## Sending is opt-in

The four compose actions are absent from the tool schema unless
`THUNDERBIRD_ALLOW_SEND=true`, and are rejected by a handler backstop even if a
client bypasses the schema. `THUNDERBIRD_READ_ONLY=true` removes all mutating and
sending actions at once.

## Credentials

Passwords are decrypted only in memory and are never logged or returned by any
tool. A master-password-protected profile cannot be decrypted; the server degrades
to read-only rather than exposing an error in place of a credential.
