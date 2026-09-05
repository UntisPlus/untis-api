# App Integration — Change Detection / Push / Subscribe

Status: implemented in v1.4.0. This is the contract for the Android app
(BetterPlus-Android) and any other client that wants live timetable changes.

## Two event sources

| Source | Type | Latency | Purpose |
|---|---|---|---|
| SSE stream | `GET /api/timetable/stream?school=S` | instant (within one poll interval) | live in-app updates |
| ntfy topic | `https://ntfy.sh/{topic}` | instant (within one poll interval) | push notification trigger |
| polling | `GET /api/timetable/changes?school=S&classId=C&since=N` | on demand | reconcile/offline catch-up |

All three are driven by the same change detection: the proxy polls each pooled
class every `-poll-interval`, diffs against the previous snapshot and bumps the
version.

## SSE stream

Requires a logged-in **session** (`JSESSIONID` cookie).

```
GET /api/timetable/stream?school=schuldorf
```

The stream always serves the **session user's own class** (`classId` is taken
from the session); the `school` query only picks the school and defaults to the
`schoolname` cookie's school. Fire-and-forget heartbeats every 30s
(`: heartbeat`). Events:

```
event: snapshot
data: {"event":"snapshot","school":"schuldorf","classId":4419,"current":4,"changes":[...]}

event: change
data: {"event":"change","school":"schuldorf","classId":4419,"version":5,"changes":[...]}
```

`socket.io`-style fallback is not needed — plain `EventSource` works. The
`school` is optional and defaults to the school on the session's `schoolname`
cookie.

## Polling diff API

```
GET /api/timetable/changes?school=schuldorf&classId=4419&since=4
```

- Requires a session whose class matches (or the `classId` param omitted → own
  class).
- `since` is the last seen version (`0` = full history).
- Returns `{school, classId, since, current, changes}`.
- `304 Not Modified` when nothing changed since `since`.

The app should drive its cache by version: keep the played-back version,
increment as `change` events arrive, and reconcile with `/changes` after
reconnect or push wake-up.

## ntfy push topics

Topics are configured server-side (admin dashboard, or self-service per-class
for ordinary users). The proxy POSTs to `ntfy.sh/{topic}` on each change. The
Android app:

1. On login, fetch the user's topics: `GET /api/ntfy?school=S` (session
   required) → `{topics:[{id,school,classId,topic}]}`.
2. Subscribe via ntfy (`https://ntfy.sh/{topic}/json` SSE, or the ntfy Android
   app).
3. On a new message, reconcile with `/api/timetable/changes`.

Topic payload: the message body is the human `X-Untis-Summary` text; the full
JSON event is delivered alongside it.

## Change row shape

Each `changes[]` entry is a `store.PeriodRow`:

```json
{
  "periodId": 123456,
  "kind": "ADDED" | "CHANGED" | "REMOVED",
  "start": "2026-09-07 08:00",
  "end":   "2026-09-07 08:45",
  "subject": "M",
  "room": "101",
  "description": "…",
  "modVer": 4
}
```

`kind` describes how the period changed relative to the previous snapshot. For
the full before/after period objects, diff `/api/timetable/changes` against the
class timetable fetched via the JSON-RPC ttservice.

## Self-service subscriptions (app-integrated config)

`/api/webhooks` and `/api/ntfy` (session required):

- `GET` — list your visible subscriptions (own class; everything with
  editor/boosted/admin).
- `POST` — create one for your class, or (privileged) for any pooled class /
  school-wide.
- `DELETE /api/webhooks/{id}` | `/api/ntfy/{id}` — remove your own (privileged
  users can remove any).

This lets the app offer in-app "notify me about changes" without admin
credentials.

## Webhook receivers

The webhook payload equals the SSE `change` data; receivers verify integrity via
`X-Untis-Signature: sha256=<HMAC-SHA256(raw body, secret)>` when a secret is
configured. See `docs/ADMIN-WEBHOOKS-NTFY.md`.