# Eufy camera proxy

A small process that logs into an Eufy account with
[`@mega-yfue/eufy-sdk`](https://github.com/mega-yfue/eufy-sdk) and re-serves
camera feeds on the machine it runs on. It is separate from the lcars-api
server.

Independent and unofficial. Not affiliated with Anker or eufy.

**Home network only.** The proxy holds an account password and serves live
video with no authentication of its own. It listens on `127.0.0.1` unless
you change `EUFY_LISTEN`. Do not expose it to the internet.

Eufy keeps one session per account. The phone app can sign this process out,
and this process can sign the app out.

## Run

Node.js 24.5 or newer. The image uses the same Node as the rest of this
repository.

```sh
cp eufy/.env.example eufy/.env   # then edit eufy/.env
# In Docker, set EUFY_LISTEN=0.0.0.0:3000 in that file.

docker build -f eufy/Dockerfile -t eufy-proxy eufy
docker run --rm -it -p 3000:3000 \
  -v "$PWD/eufy/.env:/app/.env:ro" \
  -v eufy-session:/data \
  eufy-proxy
```

The process reads `/app/.env`. The email and password are not arguments to
`docker run`, and they are not written into the image. A variable that is
already set in the environment wins over the file.

On the host, `EUFY_LISTEN` defaults to `127.0.0.1:3000` when the file omits
it. `0.0.0.0` is what makes the published container port reachable.

The first start may ask for a captcha or a 2FA code in that terminal. The
captcha image is written to the session directory (`captcha.png` on the
volume). Later starts reuse `session.json` in that directory and do not ask
again. Run it in a terminal for that first login: without one, it exits
instead of waiting.

To run it directly, with Node 24.5 or newer:

```sh
cd eufy
cp .env.example .env   # then edit .env
npm ci
node src/main.ts
```

`npm ci` in CI and `make eufy` run inside the repository's Node image.
The session directory defaults to `eufy/.session/` and is gitignored, as are
`eufy/.env` and `eufy/.eufy-captcha.png`. Never commit the password, the
`.env` file, the session file, or a captcha image. `.env.example` has
placeholders only.

## HTTP

| Method | Path | Description |
|--------|------|-------------|
| GET | `/healthz` | `loggedIn` and the serials with a client attached. |
| GET | `/cameras` | Serial, name, and whether the device has a camera. |
| GET | `/stream/<sn>` | Live video as Annex-B bytes. Opening the response starts the camera; closing it stops that viewer. |
| GET | `/snapshot/<sn>` | The latest stored JPEG. A miss is 404 and does not wake the camera. |

A battery camera's stream is extended for as long as a client is reading,
then left to stop. Wired cameras are not on a budget.

Serials are letters and digits. There is no pan, tilt, talkback, or other
control.
