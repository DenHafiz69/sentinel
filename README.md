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
