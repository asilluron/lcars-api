import assert from "node:assert/strict";
import test from "node:test";

import { annexB, type AnnexSource } from "./annex.ts";

test("annexB writes video bytes and extends the budget while open", async () => {
  const source = fake();
  const readable = annexB(source);
  source.emitVideo(Buffer.from("annex"));
  source.emitBudget();
  assert.equal(readable.read()?.toString(), "annex");
  assert.equal(source.extended, 1);

  readable.destroy();
  await new Promise((resolve) => readable.once("close", resolve));
  source.emitBudget();
  assert.equal(source.extended, 1);
  assert.equal(source.stopped, true);
});

test("annexB pauses the source when the reader falls behind", () => {
  const source = fake();
  const readable = annexB(source);
  source.emitVideo(Buffer.alloc(64 * 1024, 1));
  assert.equal(source.paused, true);
  readable.destroy();
});

function fake(): AnnexSource & {
  extended: number;
  stopped: boolean;
  paused: boolean;
  emitVideo(data: Buffer): void;
  emitBudget(): void;
} {
  let onVideo: ((frame: { data: Buffer }) => void) | undefined;
  let onBudget: ((notice: { extend: () => void }) => void) | undefined;
  const source: AnnexSource & {
    extended: number;
    stopped: boolean;
    paused: boolean;
    emitVideo(data: Buffer): void;
    emitBudget(): void;
  } = {
    extended: 0,
    stopped: false,
    paused: false,
    onVideo(listener) {
      onVideo = listener;
    },
    onBudget(listener) {
      onBudget = listener;
    },
    onError() {},
    onStop() {},
    stop() {
      this.stopped = true;
    },
    pause() {
      this.paused = true;
    },
    resume() {
      this.paused = false;
    },
    emitVideo(data) {
      onVideo?.({ data });
    },
    emitBudget() {
      onBudget?.({
        extend: () => {
          source.extended += 1;
        },
      });
    },
  };
  return source;
}
