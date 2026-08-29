# God-Api Feature Plan

Status: agreed design, not yet implemented. Implement in stages, test bit-by-bit
with the user after each stage, git-commit each stage cleanly.

## Goal

Introduce a tiered permission model for the untis-proxy backend:

1. Normal users get **reconstructed** teacher/room/subject timetables via
   per-user `ROOM`/`TEACHER`/`SUBJECT` permissions.
2. Plain teachers / non-students get **100% stock WebUntis passthrough**
   (logged + credentials saved, donated to nothing).
3. A `god-api` permission grants a **full raw superuser** view, served raw
   from the saved teacher accounts — mutually exclusive with levels 2/3.
4. A `god-api-editor` permission unlocks absence-checking + lesson/subject
   write methods, denied to everyone else.

## Permission table

| Feature | Meaning | Level |
|---|---|---|
| `ROOM` | Reconstruction of room timetables | 2 |
| `TEACHER` | Reconstruction of teacher timetables | 3 |
| `SUBJECT` | Reconstruction of subject timetables | 3 |
| `god-api` | Full raw superuser (see below); mutually exclusive with ROOM/TEACHER/SUBJECT | 5 |
| `god-api-editor` | Unlocks absences + lesson/subject writes (denied to all others) | 6 |

Automatic rules (no perm tier): class pool available to everyone; students
(person_type=5) donate to the pool automatically, non-students do not.

Enforced rule: granting `god-api` auto-revokes `ROOM`/`TEACHER`/`SUBJECT`, and
granting any of those auto-revokes `god-api`.

## Level-by-level behavior

### Level 1 — no perms (default student / new user)
- Personal timetable: own stock Untis data
- Class pool: all classes visible
- Teacher/Room/Subject: not visible (no reconst. access)
- Info-center/messages: own stock data
- Absence-checking: blocked
- Lesson/subject writes: blocked
- Donate to pool: student yes / non-student no

### Level 2/3 — ROOM / TEACHER / SUBJECT (reconstruction path)
- Each granted type toggles visibility, **always served by 100% reconstruction**
  from pooled class data; never touched by god-api.
- Absence/writes: blocked
- Donate to pool: student yes / non-student no
- Caveat: reconstruction only serves elements present in pooled class data.

### Level 4 — plain teacher / non-student (no perms)
- Login logged; credentials saved (becomes a "teacher account lying around")
- Everything: 100% stock WebUntis passthrough (nothing touched)
- Personal timetable: own stock data
- Absence/writes: blocked
- Donate to pool: no (class_id not written)
- These saved teacher accounts are the raw source god-api draws from.

### Level 5 — god-api (full raw superuser)
- Requirement: user with a replayable secret; cannot hold ROOM/TEACHER/SUBJECT
- Donate to pool: always (non-student override)
- Teacher/Room/Class/Student timetables: ALL served raw, aggregated from the
  saved teacher accounts
- MasterData: real upstream (full teachers/rooms/classes/subjects)
- Info-center: raw
- Absence-checking: blocked (needs god-api-editor)
- Lesson/subject writes: blocked (needs god-api-editor)

### Level 6 — god-api-editor
- Absence-checking: allowed
- Lesson/subject/timetable writes: allowed
- Without it: absences + all write methods blocked for every role.

## Enforcement rules

- Mutual exclusion in the store + CLI.
- Donation: ClassID written at login only for students (person_type=5) or
  god-api holders; others stored with ClassID=0.
- Sensitive block: absence + write methods denied unless god-api-editor; enforced
  in every forwarding path (JSON-RPC self-auth + session, legacy JSON-RPC, REST).
- Data source: ROOM/TEACHER/SUBJECT → reconstruction; god-api → raw from saved
  teacher accounts. Non-overlapping.

## Files to change (implementation phases)

- `internal/store/store.go` — mutual exclusion in SetReconOverride/SetReconType,
  a `GodSourceAccounts()` helper (saved teacher accounts with secrets),
  donation logic.
- `internal/proxy/jsonrpc_intern.go` — donation gate at keyLogin; god-mode raw
  branch in getTimetable2017; sensitive-method denylist in the default path.
- `internal/proxy/jsonrpc.go`, `internal/proxy/rest.go` — denylist in
  passthrough/REST; god-mode raw for weekly REST elements.
- `cmd/untisctl/main.go`, `cmd/perm/main.go` — add god-api, god-api-editor to
  grantable features + auto-revoke logic + perms list display.
- `README.md`, `docs/APP-INTEGRATION.md` — document the tiers.

## Implementation notes / decisions flagged

1. Reconstruction source limit: since non-students no longer donate, the
   reconstruction pool shrinks to students only. If no student accounts are
   provisioned/pooled, level 2/3 reconstruction has little data. Decide whether
   some teacher accounts should still be donated to seed reconstruction (as pool
   data, not for god).
2. Raw-via-teacher-accounts for god-api: aggregation may hit rate limits if one
   teacher account serves many concurrent requests; plan session-cache reuse /
   small rotation.

## Staged implementation order (agreed)

- Stage A: donation rules (students donate, non-students don't unless god-api) +
       mutual exclusion in store/CLI.
- Stage B: sensitive-method denylist (absences + writes) for all unless
       god-api-editor; add god-api-editor to CLI.
- Stage C: god-api raw serving from saved teacher accounts (JSON-RPC + REST).
- Stage D: docs + end-to-end verification.

Test bit-by-bit with the user after each stage; commit each stage cleanly.
