# One-liner install — design

Date: 2026-07-21
Status: approved

## Goal

Offer a copy-paste one-liner that installs the latest released `thunderbird-mcp`
binary on Linux and macOS:

```bash
curl -fsSL https://mudler.github.io/thunderbird-mcp/install.sh | sh
```

## Design

### `docs/install.sh`

A POSIX-sh installer. It lives in `docs/` so the existing mkdocs GitHub Pages
deploy serves it at `https://mudler.github.io/thunderbird-mcp/install.sh`
(mkdocs copies non-markdown files in the docs dir into the site verbatim — no
pipeline changes). The raw.githubusercontent URL works as a fallback.

Behavior:

- Detect platform: `uname -s` → `linux`/`darwin` (anything else, including
  Windows shells, errors with a pointer to the GitHub releases page);
  `uname -m` → `amd64`/`arm64`.
- Resolve the release tag: `VERSION=vX.Y.Z` env override, otherwise the
  `tag_name` of `releases/latest` from the GitHub API (parsed with grep/sed,
  no jq dependency).
- Download `thunderbird-mcp_<version>_<os>_<arch>.tar.gz` (GoReleaser strips
  the leading `v` in asset names) plus `checksums.txt` into a `mktemp -d`
  directory that is removed on exit, using `curl` or `wget` (whichever
  exists).
- Verify the archive's sha256 against `checksums.txt` (`sha256sum` or
  `shasum -a 256`); mismatch is a hard failure, nothing is installed.
- Extract and install the binary to `$HOME/.local/bin` by default; `BINDIR`
  overrides (e.g. `BINDIR=/usr/local/bin`; the script uses sudo only if the
  target directory is not writable). Warn if the target dir is not on `PATH`.
- Finish by printing the installed tag, the install path, and a
  `THUNDERBIRD_PROFILE` quick-start hint. The script does not execute the
  installed binary (it is an MCP stdio server; running it would try to open a
  profile).
- `set -eu`; clear error messages for unsupported platform, missing
  downloader, failed API resolution, and checksum mismatch.

### Docs

- README "Quick start" gains the one-liner as the first option.
- `docs/index.md` gets the same one-liner in its quick-start section.

## Non-goals

- Windows support in the script (releases page has zips).
- Package managers (Homebrew, AUR, …).
- Self-update; re-running the script upgrades in place, which is enough.

## Testing

- `shellcheck docs/install.sh` clean.
- End-to-end run against the latest published release (v0.1.0): default
  install into a temp `BINDIR`, checksum verified, binary present and
  executable; also exercise `VERSION` override and checksum-mismatch failure
  path.
