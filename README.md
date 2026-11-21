# Sentinel

Telegram-style web chat for [Hermes Agent](https://github.com/NousResearch/hermes-agent),
self-hosted on your own machine and published safely through a Cloudflare Tunnel
with Cloudflare Access as the only login.

One static Go binary, no database, no frontend build step, no Node.

## What you get

- Threads like a messenger: `/new` starts a fresh chat, the sidebar keeps the rest,
  every thread is a named Hermes session you can also resume from the CLI
  (`hermes -c webchat-...`)
- Streaming replies, a thinking indicator with elapsed time, and a Stop button
- Send images and files; the agent examines images with its vision tool and reads
  documents straight off the paths you attach
- The agent ships files back to you: any deliverable it mentions (chart, PDF,
  spreadsheet, source file) is detected in the reply and delivered as a download
  chip, mirroring Hermes gateway deliverable mode
- Installable as a PWA ("Sentinel" on your home screen, service worker caches the
  shell so it opens offline)
- Login is Cloudflare Access (one-time PIN, Google, GitHub, whatever your Zero
  Trust team allows). The origin also validates the Access JWT itself and fails
  closed, so a misconfigured or missing Access app can never silently expose the
  chat
- `/update` in the composer: pulls, runs the whole quality gate, swaps the
  binary and restarts the service, then the tab reconnects by itself

## How it works

```
browser ──HTTPS── Cloudflare edge (Access login + JWT)
                      │ tunnel (cloudflared, outbound-only)
                      ▼
               Sentinel (Go, 127.0.0.1:9121)
                      │ per message, single worker
                      ▼
        hermes chat --query-file -Q --format stream-json
                    --continue <session name> --create-if-missing
```

- Each thread maps to a named Hermes session; conversation state lives in the
  agent's own session store. Sentinel only keeps a small `state.json` with the
  thread list and rendered messages.
- Replies arrive as newline-delimited JSON events; `text` deltas are relayed to
  the browser over SSE while the agent is still working.
- Images are passed the same way the Hermes gateway does it: the absolute path
  plus a hint to examine it with the `vision_analyze` tool. One-shot mode does
  not accept `--image` in current Hermes builds.
- Generated files are detected by scanning the final reply for absolute paths
  with known extensions, ignoring anything inside code blocks. Downloads are
  served through `os.Root` scoped to your home, one data mount and `/tmp`, so
  traversal and symlink escapes are blocked by the kernel, not by string checks.

## Gateway delivery (cron results in a thread)

Sentinel is a delivery sink for the Hermes gateway, so scheduled work can land
here instead of only in Telegram. The gateway's `sentinel` platform plugin
(`plugins/sentinel/`, installed as `~/.hermes/plugins/sentinel`) posts to:

```
POST /api/inject          X-Sentinel-Token: <secret>
{"thread": "t3", "text": "...", "role": "assistant", "title": "Disk digest"}
```

Sentinel appends the message to that thread, broadcasts it on `/api/events`, and
any open tab renders it like a normal reply. A job is addressed with a thread id:

```bash
hermes cron edit <job_id> --deliver "telegram:244300056,sentinel:t3"
```

Guard rails, because this endpoint is necessarily outside the Access gate:

- loopback `RemoteAddr` only, and any request carrying `Cf-Connecting-Ip`,
  `Cf-Ray`, `X-Forwarded-For` or `X-Real-Ip` is refused, so the public hostname
  cannot reach it even though cloudflared dials 127.0.0.1;
- a 32-byte shared secret, auto-generated at `~/.config/sentinel/inject.token`
  (0600) on first run, compared in constant time;
- it never starts an agent run: content is appended verbatim, so a delivery
  cannot be turned into an instruction that the agent executes.

Thread ids are the `threads[].id` values from `/api/state` (`t1`, `t2`, ...).

## Updating from the chat

`/update` runs the whole deploy loop on the host that runs Sentinel:

1. `git fetch`, then `git merge --ff-only origin/<branch>`. It refuses on a
   detached HEAD or a checkout with uncommitted tracked changes, so it can never
   rewrite local work.
2. Quality gate: `gofmt -l`, `go vet`, `go test`, `staticcheck`, `gosec`.
3. `go build` into `bin/webchat.new`; the file is renamed over `bin/webchat`
   only after every gate passed, so a failed build never leaves a broken binary.
4. Restart through `systemd-run --user`, so the restart job lives outside
   Sentinel's own cgroup and survives the stop it triggers. If nothing changed
   but a source file is newer than the binary, it rebuilds and restarts; if the
   tree is already current it just says so.

The checkout is resolved from the running binary (`<repo>/bin/webchat`), so an
update can only ever touch the tree Sentinel was built from. Failures are
written into the thread as error messages; the old process keeps serving.

## Setup

Prerequisites: Go 1.24+ (uses `os.Root`), the `hermes` CLI installed and
authenticated, cloudflared with a login cert, a Cloudflare zone on your domain.

```bash
go build -o bin/webchat .
./bin/webchat   # binds 127.0.0.1:9121; WEBCCHAT_DEV=1 skips the auth gate for local tests
```

Configuration via environment:

| Variable | Meaning | Default |
|---|---|---|
| `WEBCCHAT_ADDR` / `WEBCCHAT_PORT` | bind address | `127.0.0.1:9121` |
| `WEBCCHAT_DATA` | state + uploads directory | `<repo>/data` |
| `CF_ACCESS_TEAM` | Zero Trust team name for JWT validation | none, required in prod |
| `CF_AUD` | Access application AUD tag (Cloudflare generates a hex tag per app, it is not the domain) | none, required in prod |
| `WEBCCHAT_DEV` | `1` disables the Access JWT gate. Never enable while exposed | unset |
| `SENTINEL_UNIT` | systemd user unit that `/update` restarts | `hermes-webchat` |
| `SENTINEL_INJECT_TOKEN` | shared secret for `POST /api/inject`; overrides the token file | unset |
| `SENTINEL_TOKEN_FILE` | where the inject token lives | `~/.config/sentinel/inject.token` |
| `SENTINEL_RUN_TIMEOUT` | limit for one chat run, Go duration; clamped to 2m–3h | `15m` |
| `SENTINEL_ALERT_TELEGRAM` | `0` turns off run-failure alerts | alerts on |
| `SENTINEL_ALERT_CHAT_ID` | override the Telegram destination | `TELEGRAM_HOME_CHANNEL` |

## Run limits and failure alerts

A webchat run is a child `hermes chat` process. It is killed at
`SENTINEL_RUN_TIMEOUT`, and the chat shows
`agent run exceeded the <limit> limit and was stopped`.

When a run fails for any reason (timeout, crash, provider error) the server also
posts a short plain-text alert to Telegram, so a failure that happens while the
tab is closed is not lost. The message names the thread, the reason, the Hermes
session and the time; the chat itself appends `_Alert sent to Telegram._` when the
post succeeded.

- Alerts use the Hermes bot: the token is read at startup from the allowlisted keys
  of `$HERMES_HOME/.env` (`TELEGRAM_BOT_TOKEN`, `TELEGRAM_HOME_CHANNEL`) and is never
  logged. `SENTINEL_ALERT_CHAT_ID` and `TELEGRAM_BOT_TOKEN` in the environment take
  precedence.
- Throttled to one alert per thread per 5 minutes and 10 per hour overall, so a
  retry loop cannot flood the chat.
- Delivery is best effort with an 8s timeout; a failure is logged, not retried.
- `SENTINEL_ALERT_E2E=1 go test -run TestAlertE2E ./...` sends one real message to
  prove the credentials work.

Sample deployment files are in `deploy/`:
- `sentinel.service` — systemd user unit
- `cloudflared-chat.service` — tunnel unit
- `chat.yml` — cloudflared ingress config

Create the tunnel and route DNS to it, then add a Cloudflare Access
self-hosted application for the hostname. After logging in once, grab the AUD
tag from the Access app settings (or from a `journalctl` denial line, which
logs the token's actual `aud`) and set it as `CF_AUD`.

## Security notes

- The server binds to loopback only; the tunnel is the single ingress.
- Every request must carry a valid Access JWT (header, `CF_Authorization`
  cookie or `Authorization: Bearer`), verified against the team's JWKS:
  signature, issuer, expiry and the application's AUD tag. No token, no page.
- Uploads are capped at 50 MB and stored with sanitized names; only files that
  came through the upload endpoint can be attached to a message.
- Served files are constrained by `os.Root`; SVG is never rendered inline.
- The UI ships a strict-ish CSP and all rendering escapes HTML before the
  minimal markdown pass.

## Development

```bash
gofmt -l .
go vet ./...
go test ./...
go build -o bin/webchat .
staticcheck ./...   # must report nothing
gosec -quiet ./...  # must report nothing unannotated; justified exceptions carry // #nosec comments
```

Deliverable-path extraction is unit tested in `deliver_test.go`.

## License

MIT
