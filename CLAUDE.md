# CLAUDE.md

Guidance for Claude (and other AI assistants) working in this repository.

## What this is

`lcars-api` is the backend for a home assistant display: an IoT device styled
after the LCARS interface from Star Trek. The server runs on a computer on the
home network, reads data from that computer's files (such as `.ics` calendar
exports), and serves it as JSON for the device to render. It is
unauthenticated and meant only for a trusted LAN.

## This is a public, open-source project

Everything committed here is published. Treat every file, commit message, PR
description, test fixture, and code comment as public.

**Never commit:**

- Personal data of any kind: real names, email addresses, phone numbers, home
  addresses, calendar contents, family details, schedules.
- Details of the maintainer's machine or network: absolute home paths
  (`/Users/<name>/...`, `/home/<name>/...`), hostnames, LAN IP addresses,
  Wi-Fi names, device serial numbers.
- Secrets: API keys, tokens, passwords, private URLs (including private
  calendar feed URLs), even in tests or examples.
- `lcars.toml`, the local config file. It is gitignored. Only
  `lcars.example.toml` is committed, and it must contain placeholders only.

**When writing examples, docs, and test data:**

- Use obviously fictional content: "Soccer practice", "Team standup",
  `Community park`.
- Use reserved names: `example.com`/`example.org` for domains and emails,
  `192.0.2.0/24` (RFC 5737) for IP addresses, `~/Calendars` or `/srv/...` for
  paths.
- Use fixed dates in tests (the e2e fixtures use March 2030), not anything
  derived from a real calendar.
- Never copy content from the maintainer's real files into the repo, even
  "just as a test case". Write a fictional equivalent instead.

**Before committing**, review the diff (`git diff --cached`) for anything
from the lists above, and check that `lcars.toml` and other local files aren't
staged. If something personal has already been committed, stop and tell the
user rather than trying to fix history yourself.

## Security expectations

The API has no authentication, so:

- Endpoints are read-only. Don't add endpoints that write files, run
  commands, or change the host.
- Never build a filesystem path from request input. Directories come only
  from the operator's config. Read them through `os.Root` (as
  `internal/calendar` does) so nothing escapes the configured directory.
- Don't log the contents of the user's data (event titles, file contents) at
  `info` level or above. Paths and counts are fine.
- Return errors to clients through `*HTTPError` with a generic message;
  details go to the server log.

## Conventions

- Go standard library first (`net/http`, `log/slog`). Add a dependency only
  when it saves real complexity (e.g. iCalendar recurrence), prefer
  well-maintained ones, and run `make vuln` afterwards.
- Routes are all registered in `addRoutes` in `internal/server/server.go`.
  Handlers return `error`; see `internal/server/handler.go`.
- Every route must be documented in `internal/server/api/openapi.json`.
  `TestRoutesMatchOpenAPI` enforces this, and `make api-lint` validates the
  document. The rendered docs are served at `/docs`.
- Every setting is available both in the config file and as an `LCARS_*`
  environment variable (env wins). Add new settings to the `settings` table
  in `internal/config/config.go` **and** to `lcars.example.toml`;
  `TestExampleFile` checks they match. Also update the configuration table
  in `README.md`.
- New behavior gets unit tests and, if it is visible over HTTP or affects
  startup and shutdown, e2e tests in `e2e/` against the real binary (and the
  Docker image where relevant).

## Commands

```sh
make check     # everything CI runs: tidy, fmt, lint, api-lint, test, vuln, e2e
make test      # unit tests with -race
make e2e       # end-to-end tests (binary + Docker image)
make run       # run locally with text logs; reads ./lcars.toml if present
```
