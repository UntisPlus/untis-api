# untis-proxy

A Go reverse-proxy in front of WebUntis. It lets the **BetterPlus-Android** app
log in once and then:

- pool all users' classes so **every class timetable is available to everyone** (no per-user WebUntis grant needed),
- **reconstruct teacher / room / subject timetables** from the pooled class data,
- produce **per-user (personal) iCal calendar subscription URLs**,
- detect **timetable changes** and push them to the app via a live SSE stream.

It is deployed at `https://api-beta.deelabs.tech` (and, once moved, on a NAS as
`https://api-untis.deelabs.tech`).

---

## Quick start (local)

```sh
go build ./cmd/server
./untis-server -db data/untis.db -env dev
curl http://127.0.0.1:8787/status
```

The server needs a populated database (users + secrets). See **Provisioning**
below.

---

## Configuration

Flags (all also available as `UNTIS_*` env vars):

| Flag | Default | Meaning |
|------|---------|---------|
| `-addr` | `:8787` | listen address |
| `-db` | `untis.db` | sqlite database path |
| `-server` | `schuldorf.webuntis.com` | fallback upstream host (auto-resolved per school) |
| `-school` | `schuldorf` | school name (the key data is stored under) |
| `-env` | `dev` | deployment mode: `dev` / `beta` / `prod` |
| `-version` | `dev` | reported build version |
| `-ttl` | `5m` | timetable cache TTL |
| `-poll-interval` | `60s` | how often the change-detector polls each class |
| `-year-start` / `-year-end` | auto | school-year bounds |

The upstream host for a school is **auto-resolved** via WebUntis'
`searchSchool` API, so `-server` is only a fallback. The **school name** is the
identifier for sessions, the class pool, recon, perms and calendar tokens, so
keep it consistent with the database you migrated.

---

## API endpoints

**Status**

```
GET /status
{"mode":"prod","status":"ok","uptime_sec":6,"version":"v1.2.0"}
```

**Calendar subscriptions** (token-based, stable)

```
POST /api/calendar/token      # request a class or personal token
GET  /api/calendar/{token}.ics   # the iCal feed (Outlook/Google subscribe)
```

`POST /api/calendar/token` requires a logged-in session.

- `{"classId": <id>}` → a token bound to the **class** timetable (everyone in the pool).
- `{"personal": true}` → a personal **STUDENT** token for the session user
  (their real per-student schedule, fewer lessons than the whole class).

**Timetable change detection**

```
GET /api/timetable/changes?since=<version>
GET /api/timetable/stream      # SSE live stream
```

The server polls each pooled class every `-poll-interval`, diffs it against the
last stored snapshot, and bumps the version. Changes are persisted and pushed
live over the SSE stream (`event: change`).

---

## `untisctl` — operator CLI

A single tool to manage the database (perms, users, pool, tokens, status)
instead of stacking flags. It edits the SQLite DB directly, so it can run
against a live server's data file.

```sh
go build ./cmd/untisctl
./untisctl -db data/untis.db <command>
```

(The `-db` default is `untis.db`; the binary is also baked into the Docker
image as `/usr/local/bin/untisctl`.)

### perms — who can reconstruct what

Access is **per element type** (`TEACHER` / `ROOM` / `SUBJECT`), each with a
**global switch** that applies to everyone **plus a per-user override** that
wins when set. The **class pool is available to everyone by default**.

```sh
untisctl perms list                        # show global switches + overrides
untisctl perms grant --global room         # enable ROOM reconstruction for everyone
untisctl perms grant --user Evadee teacher # per-user override
untisctl perms revoke --user Evadee subject
untisctl perms clear --user Evadee         # drop overrides -> fall back to global
untisctl perms reset                       # wipe all -> class-pool-only for everyone
```

> Flags come **before** the positional type: `perms grant --user X teacher`
> (Go's `flag` package does not intersperse flags after positionals).

In the app, classes are always shown; **teacher/room/subject element types are
hidden unless the user is granted them** (the masterData `displayAllowed` flags
are set from the user's effective access).

### users — accounts and secrets

```sh
untisctl users list
untisctl users add --user Linoth --secret <base32-key> --method key
untisctl users add --user X --secret <password> --method password
untisctl users remove --user Linoth   # also removes secret, perms, personal tokens
```

Users store either a password (`method=password`) or a **base32 TOTP secret**
(`method=key`, 16 chars, e.g. `TX6K4WLEQ4P4CG2L`). The server logs these
accounts into WebUntis on their behalf.

### pool — classes

```sh
untisctl pool list   # pooled classes + their owner account
```

A class is in the pool as soon as any user belongs to it (`class_id` set).

### tokens — calendar subscriptions

```sh
untisctl tokens list          # all class/personal calendar tokens
untisctl tokens revoke <tok>  # revoke one
```

### status — database stats

```sh
untisctl status
# users, pool size, recon element counts, perms rows, token count
```

---

## Provisioning new students

Once per person, provision their WebUntis account into the DB so the proxy can
authenticate as them (becomes a pool owner / can log in):

1. Add the account with `untisctl users add` (key or password).
2. Optionally grant reconstruction perms with `untisctl perms grant ...`.

The seed tool (`cmd/seed`) does the same from a `credentials.txt` INI-style file
and also fills in `person_id` / `class_id` by logging in live.

---

## Deployment — Docker Compose (NAS)

Linux host with Docker Compose. Use `compose.nas.yaml` (image is prebuilt on
Docker Hub, no build on the NAS):

```yaml
services:
  untis-proxy:
    image: datpersothere/untis-proxy:latest
    container_name: untis-proxy
    restart: unless-stopped
    ports:
      - "8509:8509"
    environment:
      UNTIS_ADDR: ":8509"
      UNTIS_ENV: "prod"
      UNTIS_VERSION: "v1.2.0"
      UNTIS_POLL_INTERVAL: "60s"
    volumes:
      - ./data:/data
    labels:
      - dockflare.enable=true
      - dockflare.hostname=api-untis.deelabs.tech
      - dockflare.service=http://untis-proxy:8509
```

### Steps

1. **Create the folder** and drop in `compose.nas.yaml`:
   ```sh
   mkdir -p ~/untis-proxy/data && cd ~/untis-proxy
   ```

2. **Migrate the existing database** (carries over pool, perms, calendar tokens,
   recon snapshot) from this machine:
   ```sh
   scp data/untis.db your-nas:~/untis-proxy/data/untis.db
   ```
   Skip this to start empty (the pool re-scans on boot, but perms/tokens are lost).

3. **Start** — it pulls `datpersothere/untis-proxy:latest` automatically:
   ```sh
   docker compose -f compose.nas.yaml up -d
   ```

4. **Verify on the LAN**:
   ```sh
   curl http://<NAS-IP>:8509/status
   ```

5. **Manage it**:
   ```sh
   docker exec untis-proxy untisctl -db /data/untis.db status
   docker exec untis-proxy untisctl -db /data/untis.db perms list
   ```

### Exposing `api-untis.deelabs.tech`

The public URL runs through a **cloudflared (Cloudflare Tunnel)** container in
`host` network mode (a dashboard-managed, token-based tunnel — not the compose
labels themselves). To move it to the NAS:

1. In Cloudflare **Zero Trust → Networks → Tunnels**, create a tunnel and add a
   public hostname `api-untis.deelabs.tech` → `http://localhost:8509`.
2. Run cloudflared on the NAS with that tunnel's token:
   ```sh
   docker run -d --name untis-tunnel --network host --restart unless-stopped \
     cloudflare/cloudflared:latest tunnel --no-autoupdate run --token <TOKEN>
   ```
3. Stop the old `untis-tunnel` on the previous host so two connectors don't
   fight over the hostname.

---

## Adding / rebuilding the Docker image

```sh
docker build -t datpersothere/untis-proxy:latest -t datpersothere/untis-proxy:v1.2.0 .
docker push datpersothere/untis-proxy:latest
docker push datpersothere/untis-proxy:v1.2.0
```

---

## Generating a personal calendar link

Log in as the student (keylogin TOTP), then request a personal token:

```
POST /api/calendar/token   {"personal": true}
```

which returns a stable URL like:

```
https://<host>/api/calendar/7aff8e9dd4707fde8c992c3902be0c96.ics
```

Paste that into **Outlook → Add calendar → From internet** (or iCloud/Google).
The feed is per-student (from WebUntis' `STUDENT` endpoint), so it only shows
that person's own lessons.

**Timing:** the server re-fetches at most `-ttl` (5 min) stale.
Calendar providers re-subscribe on their own schedule (often hours), so expect
calendar edits to appear well after the change-detection stream (which fires
within one `-poll-interval`, ~60s).
