import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { loadDotEnv } from "./env.ts";

test("loadDotEnv fills unset variables and does not override", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "eufy-env-"));
  const file = path.join(dir, ".env");
  writeFileSync(
    file,
    [
      "# account",
      "EUFY_EMAIL=you@example.com",
      "export EUFY_COUNTRY=GB",
      'EUFY_PASSWORD="correct horse"',
      "EUFY_LISTEN=127.0.0.1:3000",
      "",
    ].join("\n"),
  );

  const env: NodeJS.ProcessEnv = { EUFY_COUNTRY: "US" };
  loadDotEnv(file, env);
  assert.equal(env["EUFY_EMAIL"], "you@example.com");
  assert.equal(env["EUFY_COUNTRY"], "US");
  assert.equal(env["EUFY_PASSWORD"], "correct horse");
  assert.equal(env["EUFY_LISTEN"], "127.0.0.1:3000");
  assert.equal(process.env["EUFY_EMAIL"], undefined);
});

test("loadDotEnv ignores a missing default file", () => {
  const env: NodeJS.ProcessEnv = {};
  loadDotEnv(path.join(tmpdir(), "eufy-env-missing", ".env"), env);
  assert.equal(env["EUFY_EMAIL"], undefined);
});

test("loadDotEnv requires EUFY_ENV_FILE when it is set", () => {
  const env: NodeJS.ProcessEnv = { EUFY_ENV_FILE: path.join(tmpdir(), "no-such.env") };
  assert.throws(() => loadDotEnv(path.join(tmpdir(), ".env"), env), /env file not found/);
});
