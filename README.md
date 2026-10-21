# lcars-api

LCARS for home. A lightweight Go server that powers a home assistant display
styled after the LCARS interface from Star Trek.

The server runs on a computer on your home network (a Mac mini, a Raspberry
Pi, a NAS), reads data from files on that machine, such as calendar exports,
and serves it to the display over [NATS](https://nats.io) as
[protobuf](https://protobuf.dev) messages. It runs its own NATS server, so
there is nothing else to install. Generated Rust types
([prost](https://github.com/tokio-rs/prost)) for the display's client are in
[`rust/`](rust/).

A small HTTP server runs alongside for health checks, API docs, and JSON
versions of the endpoints.

> **No authentication.** Neither NATS nor HTTP is authenticated. The API is
> meant for a trusted home network only. Don't expose it to the internet.

## Quick start

Requires Go 1.27+ (and Docker for code generation and the Rust crate).

```sh
cp lcars.example.toml lcars.toml  # then edit lcars.toml (it is gitignored)
make run                          # NATS on :4222, HTTP on :8080
curl localhost:8080/healthz       # {"status":"ok","version":"(devel)","nats":"connected"}
```

Run `make` to list all targets. `make check` runs the same checks as CI.

With the [`nats` CLI](https://github.com/nats-io/natscli) you can see the
service and send an empty request (meaning "the next 7 days"):

```sh
nats --server nats://localhost:4222 micro info lcars
nats --server nats://localhost:4222 request lcars.calendar.get ''   # protobuf reply
```

## NATS API

Devices connect to `nats://<server>:4222`. Every message body is a protobuf
message from [`proto/lcars/v1`](proto/lcars/v1/calendar.proto), which documents
each field.

| Subject                  | Kind          | Body                                         |
|--------------------------|---------------|----------------------------------------------|
| `lcars.calendar.get`     | request/reply | `GetCalendarRequest` → `GetCalendarResponse`. See [Calendar](#calendar). |
| `lcars.calendar.updated` | event         | `CalendarUpdated`, published when the `.ics` files change. Request the calendar again when you get one. |

- **Errors.** A failed request gets a reply with an empty body and two
  headers: `Nats-Service-Error-Code`, an HTTP-style status (`400` for an
  invalid request, `503` if the data source isn't configured, `500` for
  anything else), and `Nats-Service-Error`, a message.
- **Discovery.** The API is a standard [NATS service](https://github.com/nats-io/nats.go/tree/main/micro)
  named `lcars`, so it answers `$SRV.PING.lcars`, `$SRV.INFO.lcars` (version
  and endpoints) and `$SRV.STATS.lcars`.
- **Compatibility.** Within `lcars.v1`, field numbers and types never change;
  new fields get new numbers, and clients ignore fields they don't know.
  Incompatible changes would go into a new `lcars.v2` package.

### Rust client

The [`lcars-proto`](rust/) crate has the generated message types, the subject
names, and the error header names. It works without `std` (with `alloc`) for
microcontrollers:

```toml
[dependencies]
lcars-proto = { git = "https://github.com/asilluron/lcars-api", rev = "<commit>" }
# no_std: add default-features = false
```

```rust
use lcars_proto::prost::Message;
use lcars_proto::{headers, subjects, v1};

let req = v1::GetCalendarRequest { from: String::new(), days: 7 }; // from "" = today
let reply = client.request(subjects::CALENDAR_GET, req.encode_to_vec().into()).await?;
if let Some(code) = reply.headers.as_ref().and_then(|h| h.get(headers::ERROR_CODE)) {
    // error reply, e.g. "400"; the message is in headers::ERROR
}
let calendar = v1::GetCalendarResponse::decode(reply.payload)?;
```

[`rust/examples/agenda.rs`](rust/examples/agenda.rs) is a complete client
using [async-nats](https://github.com/nats-io/nats.rs). It prints the
calendar, and with `--watch` prints it again on every `lcars.calendar.updated`.

Go types are in [`gen/go/lcars/v1`](gen/go/lcars/v1) (package `lcarsv1`).
Any other language can generate its own from `proto/`.

## HTTP API

Full HTTP reference: run the server and open <http://localhost:8080/docs>.

| Method | Path            | Description                                       |
|--------|-----------------|---------------------------------------------------|
| GET    | `/healthz`      | Health check. Returns `status`, `version` and `nats` (the NATS connection state); `503` while NATS is disconnected. |
| GET    | `/calendar`     | JSON version of `lcars.calendar.get`. See [Calendar](#calendar). |
| GET    | `/openapi.json` | OpenAPI 3.1 document for the HTTP API.            |
| GET    | `/docs`         | HTTP API reference ([Redoc](https://github.com/Redocly/redoc)) rendered from `/openapi.json`. |

The OpenAPI document is hand-written in `internal/server/api/openapi.json` and
embedded in the binary; `info.version` is filled in with the build version when
served. The docs page loads Redoc from jsDelivr, pinned to one version and
checked with Subresource Integrity, so the browser viewing it needs internet
access (the server itself does not).

## Calendar

`lcars.calendar.get` (NATS) and `GET /calendar` (HTTP) serve the events from a
directory of iCalendar (`.ics`) files on the host, grouped by day and ready to
draw. Both take a first day (`from`) and a number of days (`days`) and return
the same data; over NATS, times are Unix seconds (`start_unix`) instead of
RFC 3339 strings, and unset optional fields stand in for `null`. The JSON
version:

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

- `from` defaults to today and `days` to 7 (maximum 62; over NATS, `0`
  means the default). Every day in the range is listed, even with no events.
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
again every `calendar.refresh_interval`, one hour by default. When a reload
finds that files were added, removed or changed, the server publishes
`lcars.calendar.updated`. To keep it current, export from your calendar app
into that directory, or have a scheduled job (cron, launchd) download your
calendars' `.ics` feeds into it.

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
| `addr`                      | `LCARS_ADDR`                      | `:8080`          | HTTP listen address |
| `log_level`                 | `LCARS_LOG_LEVEL`                 | `info`           | `debug`, `info`, `warn` or `error` |
| `log_format`                | `LCARS_LOG_FORMAT`                | `json`           | `json` or `text` |
| `shutdown_timeout`          | `LCARS_SHUTDOWN_TIMEOUT`          | `10s`            | Grace period for in-flight requests on SIGINT/SIGTERM |
| `timezone`                  | `LCARS_TIMEZONE`                  | host timezone    | IANA name, e.g. `Europe/Berlin`. Decides what "today" is and how event times are shown |
| `calendar.dir`              | `LCARS_CALENDAR_DIR`              | unset (disabled) | Directory of `.ics` files. `~/` is expanded; a relative path in the file is relative to the file |
| `calendar.refresh_interval` | `LCARS_CALENDAR_REFRESH_INTERVAL` | `1h`             | How often to re-read `calendar.dir`. Minimum `1s` |
| `nats.addr`                 | `LCARS_NATS_ADDR`                 | `:4222`          | Listen address of the embedded NATS server. Ignored when `nats.url` is set |
| `nats.url`                  | `LCARS_NATS_URL`                  | unset (embedded) | Use an existing NATS server instead of the embedded one, e.g. `nats://192.0.2.10:4222` (comma-separate several). If it is unreachable at startup, the server exits; later it reconnects on its own |
| `nats.tls_cert`             | `LCARS_NATS_TLS_CERT`             | unset            | PEM certificate for the embedded server. With `nats.tls_key`, clients use `tls://`. Ignored when `nats.url` is set |
| `nats.tls_key`              | `LCARS_NATS_TLS_KEY`              | unset            | PEM private key matching `nats.tls_cert` |
| `nats.tls_hosts`            | `LCARS_NATS_TLS_HOSTS`            | unset            | Comma-separated DNS names and IPs that must be in the certificate SAN. When `tls_cert` is unset, a CA and certificate are created in `nats.tls_dir` on first start and reused. Delete that directory to mint a new CA |
| `nats.tls_dir`              | `LCARS_NATS_TLS_DIR`              | unset            | Directory for the generated `ca.pem`, `ca.key`, `server.pem` and `server.key`. Devices trust `ca.pem` |

## Docker

```sh
docker build -t lcars-api .
docker run -d -p 4222:4222 -p 8080:8080 \
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
proto/lcars/v1/        protobuf definitions: the NATS API contract
gen/go/lcars/v1/       generated Go types (make proto; don't edit)
rust/                  lcars-proto crate: generated Rust types (rust/src/gen), subjects, example client
internal/broker/       embedded NATS server, or connection to an external one
internal/natsapi/      NATS service: subjects and handlers
internal/calendar/     reads .ics files, expands recurring events, groups them by day
internal/config/       config file and environment variables
internal/server/       HTTP routes, handlers, middleware
internal/server/api/   OpenAPI document and Redoc docs page (embedded)
e2e/                   black-box tests: binary, Docker image, Rust client
```

NATS endpoints are registered in one place, `addEndpoints` in
`internal/natsapi/natsapi.go`, and every subject is listed in
`natsapi.Subjects`. A new subject must also be added to `rust/src/lib.rs`
and to the NATS API table above; tests check both.

To change the messages, edit `proto/lcars/v1/*.proto` and run `make proto`,
which lints the definitions and regenerates `gen/go` and `rust/src/gen` (with
[Buf](https://buf.build), via Docker). Commit the generated code; CI fails if
it is out of date. Follow the compatibility rules in the `.proto` file.

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
make e2e       # end-to-end tests: binary, Docker image, Rust client
make rust      # Rust crate: fmt, clippy, tests, no_std build (via Docker)
make api-lint  # validate the OpenAPI document (Redocly CLI, via Docker)
make check     # everything CI runs
```

**All new development must be covered by end-to-end tests.** Unit tests are
welcome too, but a feature or fix that is visible from outside the process
isn't done until an e2e test exercises it against the real binary. The suite
enforces part of this: a full `make e2e` run fails if any HTTP path in the
OpenAPI document, any NATS subject, or any setting is never used by an e2e
test.

The e2e suite (`e2e/`, behind the `e2e` build tag) builds the real binary and
tests it as a black box:

- **NATS:** the calendar over NATS matches the fixtures and the HTTP version
  field for field; error replies and headers; `lcars.calendar.updated` on
  file changes, and only then; service discovery; concurrent clients; an
  external NATS server that restarts (with `/healthz` reporting it and the
  server reconnecting).
- **HTTP:** routing, JSON request logs, the docs and OpenAPI document.
- **Process:** graceful shutdown on SIGTERM/SIGINT (HTTP and NATS ports
  closed), exit codes for bad config, a busy HTTP or NATS port, or an
  unreachable NATS server, and config file discovery and precedence.
- **Calendar:** fictional `.ics` fixtures in `e2e/testdata/calendars`
  (recurrence across a DST change, exceptions, overrides, a broken file) and
  live reloading as files change.
- **Docker:** builds the image and checks that the container runs as
  non-root, serves the calendar over NATS and HTTP from mounted files, and
  exits 0 on `docker stop`.
- **Rust client:** builds `rust/examples/agenda.rs` in a Rust container and
  runs it against the image over a Docker network: it must print the fixture
  week, report a `400` error reply, and print the calendar again after a file
  changes.

The Docker and Rust client tests are skipped when no Docker daemon is
running, unless `E2E_REQUIRE_DOCKER=1` is set (as in CI). The first Rust
client run takes a few minutes to compile; later runs reuse the build from
named Docker volumes (`lcars-cargo-*`), shared with `make rust`.

## Tooling

- **Dependencies:** kept minimal. Beyond the standard library: [nats-server](https://github.com/nats-io/nats-server) (embedded) and [nats.go](https://github.com/nats-io/nats.go) for NATS, [protobuf-go](https://github.com/protocolbuffers/protobuf-go) for messages, [BurntSushi/toml](https://github.com/BurntSushi/toml) for the config file, and [emersion/go-ical](https://github.com/emersion/go-ical) with [teambition/rrule-go](https://github.com/teambition/rrule-go) for parsing `.ics` files and expanding recurring events. The Rust crate depends only on [prost](https://github.com/tokio-rs/prost).
- **Protobuf:** [Buf](https://buf.build) (`buf.yaml`, `buf.gen.yaml`), run in Docker, lints `proto/` and generates Go (protoc-gen-go) and Rust (prost) code with pinned plugin versions.
- **Lint and format:** [golangci-lint](https://golangci-lint.run) v2 (`.golangci.yml`), with gofumpt and goimports.
- **API docs:** OpenAPI 3.1, linted with [Redocly CLI](https://redocly.com/docs/cli) (`redocly.yaml`) and rendered with Redoc at `/docs`.
- **Vulnerability scan:** `govulncheck`, pinned in `go.mod` as a tool and run with `go tool govulncheck`.
- **Container image:** multi-stage `Dockerfile` that produces a static binary on distroless `nonroot`.
- **CI:** GitHub Actions runs tests (with `-race`), lint, OpenAPI and protobuf lint, a check that generated code is current, the Rust crate checks, govulncheck and the e2e suite, including the Docker image and the Rust client. Dependabot keeps Go modules, Rust crates, Actions and base images up to date.

## Contributing

This is a public repository. Never commit `lcars.toml` or anything personal:
real calendar contents, names, home paths, hostnames or IP addresses, or
secrets such as private calendar feed URLs. Use fictional data in examples and
tests (see `e2e/testdata/calendars`). [`CLAUDE.md`](CLAUDE.md) has the full
rules and project conventions, for people and AI assistants alike.

Every change that affects behavior needs an e2e test (see [Testing](#testing)).
Run `make check` before opening a pull request.

## License

[MIT](LICENSE)
