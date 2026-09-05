# Admin Dashboard · Webhooks · ntfy · Multi-School

Status: implemented in v1.4.0. This doc covers the new surfaces only; the tiered
permission model stays as described in `GOD-API-PLAN.md`.

## Admin dashboard

A single-page UI, served **only to admin sessions** at:

```
GET /admin            → the dashboard (embedded HTML, dependency-free)
GET /admin/* …        → JSON API consumed by the dashboard
```

`/admin` itself (no trailing slash) serves the HTML page; everything under
`/admin/` is the JSON API. Both are gated by an admin **session** (JSESSIONID)
plus the `users.admin` flag.

### Bootstrapping the first admin

The DB has no admin until one is granted:

```sh
untisctl -db data/untis.db users admin --user evan          # on, default
untisctl -db data/untis.db users admin --user evan --off    # revoke
```

At server start the same can be done idempotently with the `-admin` flag:
`-admin evan,laugrü`. The flag **only** bootstraps admins that do not exist yet
(settings row `admin_bootstrap`); afterwards the `users.admin` column is the
source of truth and the dashboard can promote/demote.

An admin can do everything `untisctl` can, from the dashboard:

| Endpoint | Methods | Purpose |
|---|---|---|
| `/admin/status` | GET | counts: users, admins, pool, schools, tokens, webhooks, ntfy, recon, boosted, editors, global perms |
| `/admin/users` | GET/POST | list users; add/promote (`{username, admin}`) |
| `/admin/users/{name}` | POST/DELETE | set admin flag, set a feature (`feature`+`allowed`), revoke a permission, or delete |
| `/admin/perms` | GET | all permission rows |
| `/admin/pool[/{school}]` | GET | pooled classes + owners |
| `/admin/tokens[/{school}]` | GET | calendar tokens |
| `/admin/tokens/{token}` | DELETE | revoke a token |
| `/admin/schools` | GET | registered schools |
| `/admin/schools/{school}` | POST | register a school |
| `/admin/webhooks` | GET/POST | list / add (`school`, `classId` (0 = school-wide), `url`, `secret`) |
| `/admin/webhooks/{id}` | DELETE | remove |
| `/admin/ntfy` | GET/POST | list / add (`school`, `classId`, `topic`) |
| `/admin/ntfy/{id}` | DELETE | remove |
| `/admin/recon[/{school}]` | GET | recon snapshot stats |

## Webhooks (change subscriptions)

On every detected timetable change the proxy delivers a JSON payload to every
matching webhook:

- **school-wide** hooks (`classId = 0`) fire for every class in that school,
- **per-class** hooks fire only for their class (`classId`),

payload:

```json
{
  "event": "change",
  "school": "schuldorf",
  "classId": 4419,
  "version": 4,
  "changes": [
    { "periodId": 123456, "kind": "ADDED", "start": "2026-09-07 08:00", "end": "2026-09-07 08:45", "subject": "M", "room": "101", "description": "…", "modVer": 4 }
  ]
}
```

`kind` is `ADDED`, `CHANGED` or `REMOVED`; a summary line like
`Klasse […] M Di 1. Std neu` is in the `X-Untis-Summary` header.

Headers on every delivery:

- `X-Untis-Event: timetable-change`
- `X-Untis-Summary` — human-readable one-line change summary
- `X-Untis-Signature: sha256=<HMAC-SHA256(body, secret)>` — only if a `secret`
  was configured on the webhook; receivers should verify it.

Delivery retries 3× with backoff; a slow/unreachable webhook never blocks the
poll loop.

### Configuring webhooks

Admins: `POST /admin/webhooks` or the dashboard (Webhooks section).

Self-service (any logged-in session) at `/api/webhooks`:

| Method | Purpose | Access |
|---|---|---|
| GET | list hooks you may see | own class always; all with `editor`/`boosted`/admin |
| POST `{classId, url, secret}` | create | own class always; any pooled class + school-wide (`classId 0`) with `editor`/`boosted`/admin |
| DELETE `/api/webhooks/{id}` | remove | your own hooks; any with `editor`/`boosted`/admin |

## Push notifications via ntfy

Change events are also fanned out to ntfy topics. Topics work exactly like
webhooks: `classId = 0` is school-wide, otherwise per-class. Matched topics are
posted to `https://ntfy.sh/{topic}`; the Android app (BetterPlus-Android)
subscribes to the topic(s) the user has access to.

The pushed message body is the `summarize()` text (e.g.
`Klasse 4a: Englisch Mo 3. Stunde verlegt`), with the full JSON change payload
available in the message.

`POST /admin/ntfy` (or dashboard "ntfy topics" section) manages topics; the
same self-gating as webhooks applies at `/api/ntfy`.

### Topic naming recommendation

Use per-school prefixes so different schools never collide on the public ntfy
server, e.g. `schl{schule}-klasse{ID}` and `schl{schule}-all`. The proxy itself
only enforces per-row `school`, not topic naming.

## Streaming API (SSE) — unchanged

`GET /api/timetable/stream?school=<s>` emits `event: change`
messages on every detected change; `GET /api/timetable/changes?since=N` is the
pollable diff. Android apps can use either the SSE stream or the per-class ntfy
topic as the push trigger and reconcile via `/api/timetable/changes`.

## Multi-school

A single proxy process can serve many WebUntis schools:

- The **school name** identifies every dimension: pool, recon, master data,
  tt-cache, permissions, tokens, webhooks, ntfy topics.
- On the **first login from a school the proxy has never seen**, the school is
  auto-registered (`schools` table) and its initial recon enumeration starts in
  the background (`ensureReconScan`). No restart needed.
- The change-detection poll loop periodically re-reads the `schools` table, so
  auto-registered schools are picked up and polled automatically.
- All in-memory caches (klasses, masterData, recon element store, timetable
  cache) are per-school; cache keys carry the school prefix.
- Clients select a school with `?school=` on the login/JSON-RPC/REST requests
  (and it is persisted on the user row); a `schoolname` cookie remembers it.

Exit condition / accounting: an admin can remove a school row from the Schools
section; its data remains keyed by school name and becomes orphaned.

## Files

- `internal/proxy/admin.go` — `/admin` dashboard embed + JSON API.
- `internal/proxy/static/admin.html` — dashboard page.
- `internal/proxy/subs.go` — self-service `/api/webhooks`, `/api/ntfy`.
- `internal/proxy/notify.go` — `deliverChange`, `postWebhook`, `publishNtfy`,
  `StartPollLoop` (multi-school), changes/stream API.
- `internal/store/store.go` — `users.admin`, `schools`, `webhooks`, `ntfy_topics`
  tables + school-scoped queries.