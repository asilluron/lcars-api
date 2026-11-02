import assert from "node:assert/strict";
import test from "node:test";

import { LiveSnapshotUnavailableError, StoredSnapshotUnavailableError } from "@mega-yfue/eufy-sdk";

import { takeStill } from "./eufy.ts";

const jpeg = Buffer.from([0xff, 0xd8, 0xff, 0xd9]);

test("takeStill returns a retained push thumbnail", async () => {
  const got = await takeStill({
    snapshotStored: async () => jpeg,
    snapshotLive: async () => {
      throw new Error("live should not run");
    },
  });
  assert.deepEqual(got, jpeg);
});

test("takeStill falls back to a live burst when nothing is retained", async () => {
  const got = await takeStill({
    snapshotStored: async () => {
      throw new StoredSnapshotUnavailableError("not-observed", "No stored snapshot is available");
    },
    snapshotLive: async () => ({ jpeg }),
  });
  assert.deepEqual(got, jpeg);
});

test("takeStill returns undefined when both paths miss", async () => {
  const got = await takeStill({
    snapshotStored: async () => {
      throw new StoredSnapshotUnavailableError("not-observed", "No stored snapshot is available");
    },
    snapshotLive: async () => {
      throw new LiveSnapshotUnavailableError("no-keyframe", "no clean keyframe");
    },
  });
  assert.equal(got, undefined);
});

test("takeStill returns undefined when the camera has no still methods", async () => {
  assert.equal(await takeStill(undefined), undefined);
  assert.equal(await takeStill({}), undefined);
});
