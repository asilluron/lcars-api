import assert from "node:assert/strict";
import test from "node:test";

import { loadConfig } from "./config.ts";

const base = { EUFY_EMAIL: "you@example.com", EUFY_PASSWORD: "secret" };

test("loadConfig applies defaults", () => {
  const cfg = loadConfig(base, "/tmp/eufy-session");
  assert.equal(cfg.email, "you@example.com");
  assert.equal(cfg.password, "secret");
  assert.equal(cfg.country, "US");
  assert.equal(cfg.host, "127.0.0.1");
  assert.equal(cfg.port, 3000);
  assert.equal(cfg.sessionDir, "/tmp/eufy-session");
});

test("loadConfig reads overrides", () => {
  const cfg = loadConfig(
    {
      ...base,
      EUFY_COUNTRY: "GB",
      EUFY_LISTEN: "[::1]:3001",
      EUFY_SESSION_DIR: "/data",
    },
    "/tmp/eufy-session",
  );
  assert.equal(cfg.country, "GB");
  assert.equal(cfg.host, "::1");
  assert.equal(cfg.port, 3001);
  assert.equal(cfg.sessionDir, "/data");
});

test("loadConfig refuses a missing email or password", () => {
  assert.throws(() => loadConfig({ EUFY_PASSWORD: "secret" }, "/tmp"), /EUFY_EMAIL is required/);
  assert.throws(() => loadConfig({ EUFY_EMAIL: "you@example.com" }, "/tmp"), /EUFY_PASSWORD is required/);
});

test("loadConfig refuses a listen address without a port", () => {
  assert.throws(
    () => loadConfig({ ...base, EUFY_LISTEN: "127.0.0.1" }, "/tmp"),
    /EUFY_LISTEN must be host:port/,
  );
});
