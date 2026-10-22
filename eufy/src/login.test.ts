import assert from "node:assert/strict";
import { Readable, Writable } from "node:stream";
import test from "node:test";

import { askLine, finishLogin, type LoginSession, type LoginStep } from "./login.ts";

test("finishLogin returns when the session is already authenticated", async () => {
  let asked = 0;
  await finishLogin(session(async () => ({ status: "ok" })), async () => {
    asked += 1;
    return "";
  }, () => "/captcha.png");
  assert.equal(asked, 0);
});

test("finishLogin saves a captcha and submits the answer", async () => {
  const images: string[] = [];
  const steps: LoginStep[] = [
    { status: "captcha", image: "data:image/png;base64,YQ==", retry: false },
    { status: "ok" },
  ];
  await finishLogin(
    session(async () => {
      const next = steps.shift();
      if (!next) throw new Error("no step");
      return next;
    }, async (answer) => {
      assert.equal(answer, "abcd");
      const next = steps.shift();
      if (!next) throw new Error("no step");
      return next;
    }),
    async (message) => {
      assert.match(message, /\/tmp\/captcha.png/);
      return "abcd";
    },
    (dataURL) => {
      images.push(dataURL);
      return "/tmp/captcha.png";
    },
  );
  assert.deepEqual(images, ["data:image/png;base64,YQ=="]);
});

test("finishLogin submits a 2FA code", async () => {
  const steps: LoginStep[] = [{ status: "2fa", method: "email" }, { status: "ok" }];
  await finishLogin(
    session(async () => {
      const next = steps.shift();
      if (!next) throw new Error("no step");
      return next;
    }, async () => ({ status: "ok" }), async (code) => {
      assert.equal(code, "123456");
      return { status: "ok" };
    }),
    async (message) => {
      assert.match(message, /email/);
      return "123456";
    },
    () => "/captcha.png",
  );
});

test("finishLogin rejects an unexpected status", async () => {
  await assert.rejects(
    () => finishLogin(
      session(async () => ({ status: "nope" }) as unknown as LoginStep),
      async () => "",
      () => "/captcha.png",
    ),
    /unexpected login status/,
  );
});

test("askLine refuses a non-interactive terminal", async () => {
  const stdin = Readable.from([]) as Readable & { isTTY?: boolean };
  stdin.isTTY = false;
  await assert.rejects(
    () => askLine(stdin, new Writable(), "captcha required"),
    /run once in a terminal so the session can be saved/,
  );
});

function session(
  login: LoginSession["login"],
  solveCaptcha: LoginSession["solveCaptcha"] = async () => ({ status: "ok" }),
  submitVerifyCode: LoginSession["submitVerifyCode"] = async () => ({ status: "ok" }),
): LoginSession {
  return { login, solveCaptcha, submitVerifyCode };
}
