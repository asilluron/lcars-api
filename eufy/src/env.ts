import { existsSync, readFileSync } from "node:fs";

// Load a .env file into `env`. Variables already set there are left alone, so
// a caller can still override the file. The file is never logged. Node parses
// it (quotes, comments, `export`).
//
// `EUFY_ENV_FILE`, when set, names the file and that file must exist.
// Otherwise `file` is loaded when it is present, and ignored when it is not.
export function loadDotEnv(file: string, env: NodeJS.ProcessEnv = process.env): void {
  const explicit = env["EUFY_ENV_FILE"]?.trim();
  const path = explicit || file;
  if (!existsSync(path)) {
    if (explicit) throw new Error(`env file not found: ${path}`);
    return;
  }
  if (env === process.env) {
    process.loadEnvFile(path);
    return;
  }

  // loadEnvFile only writes process.env, and it will not replace a key that
  // is already there. Clear the file's keys for the call, then put them back.
  const names = envNames(readFileSync(path, "utf8"));
  const saved = new Map<string, string | undefined>();
  for (const name of names) {
    saved.set(name, process.env[name]);
    delete process.env[name];
  }
  try {
    process.loadEnvFile(path);
    for (const name of names) {
      const value = process.env[name];
      if (value !== undefined && env[name] === undefined) env[name] = value;
    }
  } finally {
    for (const [name, value] of saved) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  }
}

function envNames(text: string): string[] {
  const names: string[] = [];
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const match = /^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=/.exec(trimmed);
    const name = match?.[1];
    if (name) names.push(name);
  }
  return names;
}
