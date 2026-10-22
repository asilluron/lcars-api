import { createInterface } from "node:readline/promises";
import type { Readable, Writable } from "node:stream";

export type LoginStep =
  | { status: "ok" }
  | { status: "captcha"; image: string; retry: boolean }
  | { status: "2fa"; method: string };

export interface LoginSession {
  login(): Promise<LoginStep>;
  solveCaptcha(answer: string): Promise<LoginStep>;
  submitVerifyCode(code: string): Promise<LoginStep>;
}

// Drive the SDK's login result until it is authenticated. Captcha bytes are
// handed to saveCaptcha, which returns the path the operator should open.
export async function finishLogin(
  session: LoginSession,
  ask: (message: string) => Promise<string>,
  saveCaptcha: (dataURL: string) => string,
): Promise<void> {
  let step = await session.login();
  while (step.status !== "ok") {
    if (step.status === "captcha") {
      const path = saveCaptcha(step.image);
      const again = step.retry ? "incorrect, try again — " : "";
      step = await session.solveCaptcha(
        await ask(`${again}captcha required — open ${path} and type the characters`),
      );
    } else if (step.status === "2fa") {
      step = await session.submitVerifyCode(
        await ask(`2FA code sent (${step.method}) — enter it`),
      );
    } else {
      throw new Error("unexpected login status");
    }
  }
}

export async function askLine(stdin: Readable, stdout: Writable, message: string): Promise<string> {
  if (!("isTTY" in stdin) || stdin.isTTY !== true) {
    throw new Error(`${message}: run once in a terminal so the session can be saved`);
  }
  const rl = createInterface({ input: stdin, output: stdout });
  try {
    return (await rl.question(`${message}: `)).trim();
  } finally {
    rl.close();
  }
}
