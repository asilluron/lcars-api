# lcars-api

LCARS for home. A lightweight Go HTTP server that powers a home assistant
display styled after the LCARS interface from Star Trek.

The server runs on a computer on your home network (a Mac mini, a Raspberry
Pi, a NAS), reads data from files on that machine, such as calendar exports,
and serves it as simple JSON that a small IoT display can render.

> **No authentication.** The API is meant for a trusted home network only.
> Don't expose it to the internet.

## Quick start

Requires Go 1.27+.

```sh
cp lcars.example.toml lcars.toml  # then edit lcars.toml (it is gitignored)
make run                          # http://localhost:8080
curl localhost:8080/healthz       # {"status":"ok","version":"(devel)"}
```

Run `make` to list all targets. `make check` runs the same checks as CI.

## API

Full API reference: run the server and open <http://localhost:8080/docs>.

| Method | Path            | Description                                       |
|--------|-----------------|---------------------------------------------------|
| GET    | `/healthz`      | Liveness check. Returns `status` and `version`.   |
| GET    | `/calendar`     | Events from local `.ics` files, grouped by day. See [Calendar](#calendar). |
| GET    | `/openapi.json` | OpenAPI 3.1 document for this API.                |
| GET    | `/docs`         | API reference ([Redoc](https://github.com/Redocly/redoc)) rendered from `/openapi.json`. |

The OpenAPI document is hand-written in `internal/server/api/openapi.json` and
embedded in the binary; `info.version` is filled in with the build version when
served. The docs page loads Redoc from jsDelivr, pinned to one version and
checked with Subresource Integrity, so the browser viewing it needs internet
access (the server itself does not).

## Calendar

`GET /calendar` serves the events from a directory of iCalendar (`.ics`) files
on the host, grouped by day and ready to draw:

```sh
curl 'localhost:8080/calendar?from=2026-10-20&days=7'
```

```json
{
  "timezone": "America/New_York",
  "from": "2026-10-20",
  "to": "2026-10-26",
  "updated_at": "2026-10-20T08:00:00-04:00",
  "days": [
    {
      "date": "2026-10-20",
      "weekday": "Tuesday",
      "today": true,
      "events": [
        {
          "id": "4f0d7c1a@example.com/20261020T130000Z",
          "title": "Team standup",
          "calendar": "work",
          "location": "Video call",
          "all_day": false,
          "start": "2026-10-20T09:00:00-04:00",
          "end": "2026-10-20T09:15:00-04:00",
          "start_time": "09:00",
          "end_time": "09:15"
        }
      ]
    }
  ]
}
```

- `from` defaults to today and `days` to 7 (maximum 62). Every day in the
  range is listed, even with no events.
- Within a day, all-day events come first, then timed events by start time.
  An event spanning several days appears on each one. `start_time` and
  `end_time` are `HH:MM` display labels for that day, or `null` (for all-day
  events, and on days the event doesn't start or end).
- Recurring events are expanded, including exceptions and edited or cancelled
  occurrences. Times are converted to the configured `timezone`.

To set it up, point `calendar.dir` in `lcars.toml` at a directory of `.ics`
files. Subdirectories are read too. Each event's calendar name is the file's
`X-WR-CALNAME` if set, otherwise its top-level subdirectory, otherwise the file
name, so `family.ics` and `family/*.ics` both give a calendar called `family`.
Files that can't be parsed are skipped with a warning in the log.

The directory is read at startup (the server won't start if it's missing) and
again every `calendar.refresh_interval`, one hour by default. To keep it
current, export from your calendar app into that directory, or have a
scheduled job (cron, launchd) download your calendars' `.ics` feeds into it.

## Configuration

Settings come from a TOML config file and from environment variables, which
take precedence. Every setting is optional.

The server reads `lcars.toml` from its working directory if it exists, or the
file named by `LCARS_CONFIG` (which must then exist). Start from the
documented example, `lcars.example.toml`. `lcars.toml` is gitignored so your
paths stay out of the repo. Unknown keys are an error, so typos are caught at
startup.

| Config file key             | Environment variable              | Default          | Description |
|-----------------------------|-----------------------------------|------------------|-------------|
| `addr`                      | `LCARS_ADDR`                      | `:8080`          | Listen address |
| `log_level`                 | `LCARS_LOG_LEVEL`                 | `info`           | `debug`, `info`, `warn` or `error` |
| `log_format`                | `LCARS_LOG_FORMAT`                | `json`           | `json` or `text` |
| `shutdown_timeout`          | `LCARS_SHUTDOWN_TIMEOUT`          | `10s`            | Grace period for in-flight requests on SIGINT/SIGTERM |
| `timezone`                  | `LCARS_TIMEZONE`                  | host timezone    | IANA name, e.g. `Europe/Berlin`. Decides what "today" is and how event times are shown |
| `calendar.dir`              | `LCARS_CALENDAR_DIR`              | unset (disabled) | Directory of `.ics` files. `~/` is expanded; a relative path in the file is relative to the file |
| `calendar.refresh_interval` | `LCARS_CALENDAR_REFRESH_INTERVAL` | `1h`             | How often to re-read `calendar.dir`. Minimum `1s` |

## Docker

```sh
docker build -t lcars-api .
docker run -d -p 8080:8080 \
  -v "$PWD/lcars.toml:/config/lcars.toml:ro" \
  -v "$HOME/Calendars:/calendars:ro" \
  -e LCARS_CALENDAR_DIR=/calendars \
  -e LCARS_TIMEZONE=America/New_York \
  lcars-api
```

The image reads `/config/lcars.toml` if it is mounted. Paths inside the
container differ from the host's, so set `LCARS_CALENDAR_DIR` to where the
calendar directory is mounted. Set `LCARS_TIMEZONE` too, since the container
doesn't know the host's timezone.

## Project layout

```
cmd/lcars-api/         main package: wiring, signal handling, graceful shutdown
internal/calendar/     reads .ics files and expands recurring events
internal/config/       config file and environment variables
internal/server/       routes, handlers, middleware
internal/server/api/   OpenAPI document and Redoc docs page (embedded)
e2e/                   black-box tests for the binary and Docker image
```

Routes are registered in one place: `addRoutes` in `internal/server/server.go`.
Handlers have the signature `func(http.ResponseWriter, *http.Request) error`.
Return an `*HTTPError` to send a specific status code; any other error is
logged and returned to the client as a 500.

When you add or change a route, update `internal/server/api/openapi.json` too.
`TestRoutesMatchOpenAPI` fails if a route is registered but not documented, or
documented but not registered, and `make api-lint` validates the document.

## Testing

```sh
make test      # unit tests with the race detector
make e2e       # end-to-end tests against the compiled binary and Docker image
make api-lint  # validate the OpenAPI document (Redocly CLI, via Docker)
make check     # everything CI runs
```

The e2e suite (`e2e/`, behind the `e2e` build tag) builds the real binary and
tests it as a black box: HTTP behavior, JSON request logs, graceful shutdown on
SIGTERM/SIGINT, exit codes for bad config or a busy port, config file
discovery and precedence, and the calendar end to end against fictional `.ics`
fixtures in `e2e/testdata/calendars` (recurrence across a DST change,
exceptions, overrides, a broken file, and live reloading as files change). It
also builds the Docker image and checks that the container runs as non-root,
serves `/healthz` and the calendar from mounted files, and exits 0 on
`docker stop`. The Docker test is skipped when no Docker daemon is running,
unless `E2E_REQUIRE_DOCKER=1` is set (as in CI).

## Tooling

- **Dependencies:** kept minimal. Beyond the standard library: [BurntSushi/toml](https://github.com/BurntSushi/toml) for the config file, and [emersion/go-ical](https://github.com/emersion/go-ical) with [teambition/rrule-go](https://github.com/teambition/rrule-go) for parsing `.ics` files and expanding recurring events.
- **Lint and format:** [golangci-lint](https://golangci-lint.run) v2 (`.golangci.yml`), with gofumpt and goimports.
- **API docs:** OpenAPI 3.1, linted with [Redocly CLI](https://redocly.com/docs/cli) (`redocly.yaml`) and rendered with Redoc at `/docs`.
- **Vulnerability scan:** `govulncheck`, pinned in `go.mod` as a tool and run with `go tool govulncheck`.
- **Container image:** multi-stage `Dockerfile` that produces a static binary on distroless `nonroot`.
- **CI:** GitHub Actions runs tests (with `-race`), lint, OpenAPI lint, govulncheck and the e2e suite, including the Docker image. Dependabot keeps Go modules, Actions and base images up to date.

## Contributing

This is a public repository. Never commit `lcars.toml` or anything personal:
real calendar contents, names, home paths, hostnames or IP addresses, or
secrets such as private calendar feed URLs. Use fictional data in examples and
tests (see `e2e/testdata/calendars`). [`CLAUDE.md`](CLAUDE.md) has the full
rules and project conventions, for people and AI assistants alike.

Run `make check` before opening a pull request.

## License

[MIT](LICENSE)
