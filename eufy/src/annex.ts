import { Readable } from "node:stream";

// A live() consumer, narrowed to what the proxy uses. Budget notices exist on
// this event stream; openReadable() does not surface them.
export interface AnnexSource {
  onVideo(listener: (frame: { data: Buffer }) => void): void;
  onBudget(listener: (notice: { extend: () => void }) => void): void;
  onError(listener: (err: Error) => void): void;
  onStop(listener: () => void): void;
  stop(): void;
  pause(): void;
  resume(): void;
}

// Annex-B bytes from one live consumer. Destroying the readable stops that
// consumer, and a battery budget is extended only until then.
export function annexB(source: AnnexSource): Readable {
  let stopped = false;
  let paused = false;

  const readable = new Readable({
    read() {
      if (paused && !stopped) {
        paused = false;
        source.resume();
      }
    },
    destroy(err, callback) {
      if (!stopped) {
        stopped = true;
        source.stop();
      }
      callback(err);
    },
  });

  source.onVideo((frame) => {
    if (stopped) return;
    if (!readable.push(frame.data)) {
      paused = true;
      source.pause();
    }
  });
  source.onBudget((notice) => {
    if (!stopped) notice.extend();
  });
  source.onError((err) => {
    if (!stopped) readable.destroy(err);
  });
  source.onStop(() => {
    if (!stopped) readable.push(null);
  });

  return readable;
}
