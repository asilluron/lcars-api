import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";

import {
  ApiError,
  createToken,
  listTokens,
  revokeToken,
  signIn,
  type DeviceToken,
  type NewDeviceToken,
  type Session,
} from "./api";

const SESSION_KEY = "lcars-admin-session";

// The session lives in sessionStorage: it survives a reload, but not closing
// the tab. Storage can be unavailable (private windows, blocked site data),
// so every access is guarded.
function loadSession(): Session | null {
  try {
    const raw = sessionStorage.getItem(SESSION_KEY);
    if (!raw) return null;
    const s = JSON.parse(raw) as Session;
    return Date.parse(s.expires_at) > Date.now() ? s : null;
  } catch {
    return null;
  }
}

function saveSession(s: Session | null) {
  try {
    if (s) sessionStorage.setItem(SESSION_KEY, JSON.stringify(s));
    else sessionStorage.removeItem(SESSION_KEY);
  } catch {
    // The session then lasts until the page is reloaded.
  }
}

export function App() {
  const [session, setSession] = useState<Session | null>(loadSession);
  const [notice, setNotice] = useState<string | null>(null);

  const signOut = useCallback((why?: string) => {
    saveSession(null);
    setSession(null);
    setNotice(why ?? null);
  }, []);

  const onSignIn = (s: Session) => {
    saveSession(s);
    setSession(s);
    setNotice(null);
  };

  return (
    <Frame
      title={session ? "Device access" : "Admin sign-in"}
      actions={
        session && (
          <>
            <span className="who" data-testid="signed-in-as">
              {session.email}
            </span>
            <button type="button" className="pill pill-quiet" onClick={() => signOut()}>
              Sign out
            </button>
          </>
        )
      }
    >
      {session ? (
        <TokensPage session={session} onExpired={() => signOut("Your session has expired. Sign in again.")} />
      ) : (
        <SignIn notice={notice} onSignIn={onSignIn} />
      )}
    </Frame>
  );
}

/** The LCARS frame: a header bar, a sidebar of colored blocks, and content. */
function Frame({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <div className="frame">
      <header className="frame-top">
        <div className="elbow-top" aria-hidden="true" />
        <div className="top-right">
          <div className="bar">
            <h1>
              <span className="brand">LCARS</span> {title}
            </h1>
            <div className="bar-actions">{actions}</div>
          </div>
          <div className="stripe" aria-hidden="true" />
        </div>
      </header>
      <div className="frame-body">
        <nav className="sidebar" aria-hidden="true">
          <div className="block b1">01-4222</div>
          <div className="block b2">02-8080</div>
          <div className="block b3">03-ADMIN</div>
          <div className="block b4" />
        </nav>
        <main className="content">{children}</main>
      </div>
    </div>
  );
}

function SignIn({ notice, onSignIn }: { notice: string | null; onSignIn: (s: Session) => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      onSignIn(await signIn(email.trim(), password));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setPassword("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel narrow" aria-labelledby="signin-heading">
      <h2 id="signin-heading">Sign in</h2>
      <p className="muted">
        Use an admin account from this server&apos;s Supabase project. Accounts are created by the operator; see the
        README.
      </p>
      {notice && (
        <p className="message" role="status">
          {notice}
        </p>
      )}
      <form onSubmit={submit} className="stack">
        <label>
          Email
          <input
            type="email"
            autoComplete="username"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
        <label>
          Password
          <input
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        {error && (
          <p className="message error" role="alert">
            {error}
          </p>
        )}
        <button type="submit" className="pill" disabled={busy}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </section>
  );
}

function TokensPage({ session, onExpired }: { session: Session; onExpired: () => void }) {
  const [tokens, setTokens] = useState<DeviceToken[] | null>(null);
  const [created, setCreated] = useState<NewDeviceToken | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Runs an API call, signing out if the session has expired and showing
  // any other error.
  const run = useCallback(
    async <T,>(call: () => Promise<T>): Promise<T | undefined> => {
      setError(null);
      try {
        return await call();
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) onExpired();
        else setError(err instanceof Error ? err.message : String(err));
        return undefined;
      }
    },
    [onExpired],
  );

  const refresh = useCallback(async () => {
    const list = await run(() => listTokens(session.access_token));
    if (list) setTokens(list);
  }, [run, session.access_token]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const onCreate = async (name: string) => {
    const t = await run(() => createToken(session.access_token, name));
    if (!t) return false;
    setCreated(t);
    await refresh();
    return true;
  };

  const onRevoke = async (t: DeviceToken) => {
    if (!window.confirm(`Revoke "${t.name}" (${t.id})? Devices using it are disconnected immediately.`)) return;
    const ok = await run(async () => {
      await revokeToken(session.access_token, t.id);
      return true;
    });
    if (!ok) return;
    if (created?.id === t.id) setCreated(null);
    await refresh();
  };

  return (
    <>
      {error && (
        <p className="message error" role="alert">
          {error}
        </p>
      )}
      <CreateToken onCreate={onCreate} />
      {created && <NewTokenCard token={created} onDone={() => setCreated(null)} />}
      <section className="panel" aria-labelledby="tokens-heading">
        <h2 id="tokens-heading">Device tokens</h2>
        {tokens === null ? (
          <p className="muted">Loading…</p>
        ) : tokens.length === 0 ? (
          <p className="muted">No device tokens yet. Create one above for each display.</p>
        ) : (
          <TokenTable tokens={tokens} onRevoke={onRevoke} />
        )}
      </section>
    </>
  );
}

function CreateToken({ onCreate }: { onCreate: (name: string) => Promise<boolean> }) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    if (await onCreate(name.trim())) setName("");
    setBusy(false);
  };

  return (
    <section className="panel" aria-labelledby="create-heading">
      <h2 id="create-heading">New device token</h2>
      <form onSubmit={submit} className="row">
        <label className="grow">
          Device name
          <input
            required
            maxLength={100}
            placeholder="Kitchen display"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <button type="submit" className="pill" disabled={busy || name.trim() === ""}>
          {busy ? "Creating…" : "Create token"}
        </button>
      </form>
    </section>
  );
}

/** Shows a new token's credentials. They can't be shown again. */
function NewTokenCard({ token, onDone }: { token: NewDeviceToken; onDone: () => void }) {
  const host = window.location.hostname || "<host>";
  return (
    <section className="panel highlight" aria-labelledby="new-token-heading" data-testid="new-token">
      <h2 id="new-token-heading">Token for “{token.name}”</h2>
      <p className="message warn" role="status">
        Copy the password now: it is not stored, and can&apos;t be shown again. If it is lost, revoke the token and
        create a new one.
      </p>
      <dl className="credentials">
        <CopyField label="Username" value={token.id} testId="new-token-username" />
        <CopyField label="Password" value={token.secret} testId="new-token-password" />
        <CopyField
          label="HTTP header"
          value={`Authorization: Basic ${btoa(`${token.id}:${token.secret}`)}`}
          testId="new-token-header"
        />
        <CopyField
          label="NATS URL"
          value={`nats://${token.id}:${token.secret}@${host}:4222`}
          testId="new-token-nats-url"
        />
      </dl>
      <button type="button" className="pill pill-quiet" onClick={onDone}>
        Done
      </button>
    </section>
  );
}

function CopyField({ label, value, testId }: { label: string; value: string; testId: string }) {
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      // Only available in secure contexts (https, or localhost).
      await navigator.clipboard.writeText(value);
    } catch {
      input.current?.select();
      document.execCommand("copy"); // fallback for plain http on the LAN
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="credential">
      <dt>
        <label htmlFor={id}>{label}</label>
      </dt>
      <dd>
        <input
          id={id}
          ref={input}
          readOnly
          value={value}
          data-testid={testId}
          spellCheck={false}
          onFocus={(e) => e.target.select()}
        />
        <button type="button" className="pill pill-small" onClick={copy} aria-label={`Copy ${label}`}>
          {copied ? "Copied" : "Copy"}
        </button>
      </dd>
    </div>
  );
}

const dateFormat = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

function TokenTable({ tokens, onRevoke }: { tokens: DeviceToken[]; onRevoke: (t: DeviceToken) => void }) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Username</th>
            <th scope="col">Created</th>
            <th scope="col">
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {tokens.map((t) => (
            <tr key={t.id} data-testid={`token-${t.id}`}>
              <td className="name">{t.name}</td>
              <td data-label="Username">
                <code>{t.id}</code>
              </td>
              <td data-label="Created">
                <time dateTime={t.created_at}>{dateFormat.format(new Date(t.created_at))}</time>
              </td>
              <td className="actions">
                <button
                  type="button"
                  className="pill pill-small pill-danger"
                  onClick={() => onRevoke(t)}
                  aria-label={`Revoke ${t.name}`}
                >
                  Revoke
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
