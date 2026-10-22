// Config is environment only. Nothing here is written to disk, and the
// password is never logged.

export interface Config {
  email: string;
  password: string;
  country: string;
  host: string;
  port: number;
  sessionDir: string;
}

export function loadConfig(env: NodeJS.ProcessEnv, defaultSessionDir: string): Config {
  const email = required(env, "EUFY_EMAIL");
  const password = required(env, "EUFY_PASSWORD");
  const country = env["EUFY_COUNTRY"]?.trim() || "US";
  const listen = env["EUFY_LISTEN"]?.trim() || "127.0.0.1:3000";
  const sessionDir = env["EUFY_SESSION_DIR"]?.trim() || defaultSessionDir;
  const { host, port } = parseListen(listen);
  return { email, password, country, host, port, sessionDir };
}

function required(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name]?.trim();
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

export function parseListen(value: string): { host: string; port: number } {
  const match = /^(?:\[([^\]]+)\]|([^:]+)):(\d+)$/.exec(value);
  const host = match?.[1] ?? match?.[2];
  const port = match?.[3] ? Number(match[3]) : Number.NaN;
  if (!host || !Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error("EUFY_LISTEN must be host:port");
  }
  return { host, port };
}
