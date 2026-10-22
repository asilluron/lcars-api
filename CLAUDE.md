# CLAUDE.md

Guidance for Claude (and other AI assistants) working in this repository.

## What this is

`lcars-api` is the backend for a home assistant display: an IoT device styled
after the LCARS interface from Star Trek. The server runs on a computer on the
home network, reads data from that computer's files (such as `.ics` calendar
exports), and serves it to the device.

- **NATS is the primary API.** Message bodies are protobuf (`proto/lcars/v1`).
  By default the server embeds its own NATS server (`internal/broker`); the
  service and its subjects are in `internal/natsapi`.
- **The client is Rust**, using prost. The `lcars-proto` crate in `rust/`
  exports the generated types, subject names and error header names, and must
  keep working without `std` (microcontrollers).
- **HTTP runs alongside** for `/healthz` (which also reports the NATS
  connection), the docs, and JSON versions of the NATS endpoints.

Neither NATS nor HTTP is authenticated; it is meant only for a trusted LAN.

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

- Endpoints and subjects are read-only. Don't add HTTP endpoints or NATS
  handlers that write files, run commands, or change the host.
- Never build a filesystem path from request input. Directories come only
  from the operator's config. Read them through `os.Root` (as
  `internal/calendar` does) so nothing escapes the configured directory.
- Don't log the contents of the user's data (event titles, file contents) at
  `info` level or above. Paths and counts are fine.
- Return errors to clients through `*server.HTTPError` (HTTP) or
  `*natsapi.Error` (NATS) with a generic message; details go to the server
  log.
- Never log credentials: NATS URLs can contain them, so log them through
  `redact` in `internal/broker`.

## End-to-end tests are required

**All new development must be e2e tested.** A feature, endpoint, subject,
setting, or bug fix that is visible from outside the process (over NATS or
HTTP, in logs, in exit codes, in startup or shutdown, or in the Docker image)
is not done until a test in `e2e/` exercises it against the real binary.
Unit tests are welcome in addition, never instead.

- Add the e2e test in the same change as the code. Don't defer it.
- The suite enforces coverage of the API surface: a full `make e2e` run fails
  if any HTTP path in `openapi.json`, any subject in `natsapi.Subjects`, or
  any setting (env var) is never used by an e2e test. Use the recording
  helpers (`do`/`mustDo`, `natsRequest`/`requestCalendar`, `launch`) so new
  surface is counted; don't bypass the check.
- Test NATS endpoints through a real NATS client, decoding with the generated
  types, and check error replies (headers) as well as success.
- If the change affects what a Rust client sees, extend `TestRustClient`
  (`e2e/rust_test.go`) or `rust/examples/agenda.rs` so the Rust side is
  tested too. If it affects the container, extend `TestDockerImage`.
- Run `make e2e` (with Docker running) before calling work done, and report
  the result. A skipped Docker test is not a pass.

## Conventions

- Go standard library first (`net/http`, `log/slog`). Add a dependency only
  when it saves real complexity (e.g. iCalendar recurrence), prefer
  well-maintained ones, and run `make vuln` afterwards.
- NATS endpoints are all registered in `addEndpoints` in
  `internal/natsapi/natsapi.go`, and every subject (requests and events) is
  listed in `natsapi.Subjects`. A new subject must also be added to
  `rust/src/lib.rs` (`TestRustSubjects` checks) and to the NATS API table in
  `README.md` (`TestSubjectsDocumented` checks). Handlers return
  `(proto.Message, error)`.
- Subjects are `lcars.<area>.<verb>` for requests (`lcars.calendar.get`) and
  `lcars.<area>.<event>` for events (`lcars.calendar.updated`).
- When an endpoint exists over both NATS and HTTP, keep the logic shared (as
  `calendar.Store.Agenda` is) so the two can't drift; the e2e tests compare
  them.
- HTTP routes are all registered in `addRoutes` in
  `internal/server/server.go`. Handlers return `error`; see
  `internal/server/handler.go`.
- Every HTTP route must be documented in `internal/server/api/openapi.json`.
  `TestRoutesMatchOpenAPI` enforces this, and `make api-lint` validates the
  document. The rendered docs are served at `/docs`.

### Protobuf and generated code

- Messages live in `proto/lcars/v1/`. After changing them, run `make proto`
  (Buf in Docker: lint, format check, generate) and commit the regenerated
  `gen/go/` and `rust/src/gen/`. Never edit generated code by hand. CI fails
  if it is stale.
- Keep `lcars.v1` wire-compatible: never change or reuse a field number,
  never change a field's type, and add new fields with new numbers. Breaking
  changes need a new `lcars.v2` package.
- Plugin versions in `buf.gen.yaml` must match the runtime libraries:
  protoc-gen-go with `google.golang.org/protobuf` in `go.mod`, and
  neoeinstein-prost with `prost` in `rust/Cargo.toml`. Update them together.
- Prefer types that are cheap on a microcontroller: plain strings and
  integers (Unix seconds for times) over well-known types that pull in
  `prost-types`. The Rust crate must build with `--no-default-features` for
  a bare-metal target; `make rust` checks.
- Every setting is available both in the config file and as an `LCARS_*`
  environment variable (env wins). Add new settings to the `settings` table
  in `internal/config/config.go` **and** to `lcars.example.toml`;
  `TestExampleFile` checks they match. Also update the configuration table
  in `README.md`.
- New behavior gets unit tests and e2e tests; see "End-to-end tests are
  required" above.

### Cameras

- The Eufy proxy is a separate Node process in `eufy/`. It is not part of the
  Go server, NATS, or the distroless image. `make eufy` type-checks and tests
  it without an Eufy account. Account credentials live in `eufy/.env` (see
  `eufy/.env.example`); that file and the session stay out of git.

## Commands

```sh
make check     # everything CI runs: tidy, fmt, lint, api-lint, proto, test, rust, eufy, vuln, e2e
make test      # unit tests with -race
make e2e       # end-to-end tests (binary, Docker image, Rust client)
make proto     # lint proto/ and regenerate Go and Rust code (Docker)
make rust      # Rust crate: fmt, clippy, tests, no_std build (Docker)
make eufy      # type-check and test the Eufy camera proxy (Docker)
make run       # run locally with text logs; reads ./lcars.toml if present
```
