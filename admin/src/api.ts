// Client for the lcars-api admin API (see /docs on the server). Paths are
// relative, so they resolve under wherever the page is served (/admin/).

export interface Session {
  access_token: string;
  /** RFC 3339, UTC. */
  expires_at: string;
  email: string;
}

export interface DeviceToken {
  /** Also the token's username, e.g. "dev_7f3k2m9q4x8w". */
  id: string;
  name: string;
  created_at: string;
}

export interface NewDeviceToken extends DeviceToken {
  /** The token's password. Returned only when the token is created. */
  secret: string;
}

/** An error response from the API, with the server's message. */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

async function request<T>(
  method: string,
  path: string,
  opts: { token?: string; body?: unknown } = {},
): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (opts.token) headers.Authorization = `Bearer ${opts.token}`;
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";

  let resp: Response;
  try {
    resp = await fetch(path, {
      method,
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      cache: "no-store",
    });
  } catch {
    throw new ApiError(0, "Can't reach the server. Check that lcars-api is running.");
  }
  if (resp.status === 204) return undefined as T;

  const data: unknown = await resp.json().catch(() => null);
  if (!resp.ok) {
    const msg =
      data && typeof data === "object" && "error" in data && typeof data.error === "string"
        ? data.error
        : `${resp.status} ${resp.statusText}`;
    throw new ApiError(resp.status, msg);
  }
  return data as T;
}

export function signIn(email: string, password: string): Promise<Session> {
  return request("POST", "api/session", { body: { email, password } });
}

export async function listTokens(token: string): Promise<DeviceToken[]> {
  const { tokens } = await request<{ tokens: DeviceToken[] }>("GET", "api/tokens", { token });
  return tokens;
}

export function createToken(token: string, name: string): Promise<NewDeviceToken> {
  return request("POST", "api/tokens", { token, body: { name } });
}

export function revokeToken(token: string, id: string): Promise<void> {
  return request("DELETE", `api/tokens/${encodeURIComponent(id)}`, { token });
}

export type Accent = "purple" | "blue" | "orange" | "peach";

export interface Reminder {
  id: string;
  title: string;
  detail: string;
  accent: Accent;
  done: boolean;
}

export interface ReminderList {
  updated_at: string;
  reminders: Reminder[];
}

export function listReminders(token: string): Promise<ReminderList> {
  return request("GET", "api/reminders", { token });
}

export function createReminder(
  token: string,
  body: { title: string; detail: string; accent: Accent },
): Promise<Reminder> {
  return request("POST", "api/reminders", { token, body });
}

export function deleteReminder(token: string, id: string): Promise<void> {
  return request("DELETE", `api/reminders/${encodeURIComponent(id)}`, { token });
}
