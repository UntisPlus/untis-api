# Tiered Permission Model — Basic / Reconstruction / Boosted

Status: implemented and committed. Decline/stale notes removed.

## Goal

Tiered permission model for the untis-proxy backend:

1. **Basic** (default, no flag) — every user gets the pool of classes plus their
   own personal student timetable. **Everyone** (student and non-student) donates
   their class to the pool on login.
2. **Reconstruction** (flag `recon`) — teacher / room / subject timetables
   reconstructed 100% from pooled class data. Available as a global switch or a
   per-user override.
3. **Boosted** (flag `boosted`) — class/teacher/room/subject timetables
   raw-forwarded through ANY saved teacher account (`BoostedSourceAccounts`).
   The user's own personal (STUDENT) timetable stays on the user's own account.
   Info center uses the user's own account, never a teacher account. Per-user only.
4. Sub-permissions (per-user, default OFF):
   - `absences` — absence-checking methods (info center).
   - `writes` — lesson/subject write methods (set/add/update/delete/change).
   Each sub-perm **also implies Boosted raw timetable behavior**.

## Permission table

| Feature | Meaning | Global? |
|---|---|---|
| `recon` | Teacher/room/subject reconstruction from pooled classes | yes (switch + per-user override) |
| `boosted` | Raw teacher-account forwarding (except own STUDENT timetable) | no (per-user only) |
| `absences` | Absence-checking methods; implies boosted | no (per-user only) |
| `writes` | Lesson/subject write methods; implies boosted | no (per-user only) |

Enforced rule: **Boosted XOR Recon** — `boosted`/`absences`/`writes` and `recon`
are mutually exclusive per user. Granting one side auto-revokes the other.

## Tier behavior

### Basic (default)
- Personal timetable: own stock Untis data (own account).
- Class pool: all pooled classes visible, served via the pooled owner account.
- Teacher/Room/Subject: not visible.
- Absence-checking: blocked. Lesson/subject writes: blocked.
- Donate to pool: everyone (class donated at login).

### Recon (`recon`)
- Grants teacher/room/subject reconstruced timetables from pooled class data.
- Absence/writes: still blocked unless `absences`/`writes` granted.
- Caveat: only elements present in pooled class data are servable.

### Boosted (`boosted`, or implied by `absences`/`writes`)
- Class/teacher/room/subject timetables: ALL served raw, forwarded through the
  first available saved teacher account.
- Own personal (STUDENT) timetable: stays on the user's own account.
- MasterData: full upstream element lists (displayAllowed set for recon/boosted).
- Absence-checking: requires `absences`. Lesson/subject writes: requires `writes`.

### Absences (`absences`)
- Absence-checking methods allowed; also implies Boosted raw timetable behavior.

### Writes (`writes`)
- Lesson/subject/timetable write methods allowed; also implies Boosted raw
  behavior.

## Enforcement rules

- Mutual exclusion (Boosted XOR Recon) in the store + CLI.
- Donation: everyone donates their class at login.
- Sensitive block: absence methods need `absences`; write methods need `writes`;
  enforced in every forwarding path (JSON-RPC self-auth + session, legacy
  JSON-RPC passthrough, REST).
- Data source: recon → reconstruction from pooled data; boosted → raw from saved
  teacher accounts. Non-overlapping.

## Files (implementation notes)

- `internal/store/store.go` — `FeatureRecon`/`FeatureBoosted`/`FeatureAbsences`/
  `FeatureWrites`, `BoostFeatures`, `ReconAccess`/`BoostedAccess`, `setPerm`
  mutual exclusion, `BoostedSourceAccounts()`, `SetBoostedFlag`/`SetAbsencesFlag`/
  `SetWritesFlag`.
- `internal/proxy/jsonrpc_intern.go` — everyone-donates at keyLogin; boosted raw
  branch (skips STUDENT) in getTimetable2017; absence/write gating in the default
  and self-auth paths; single-recon displayAllowed.
- `internal/proxy/jsonrpc.go`, `internal/proxy/rest.go` — absence/write gating in
  passthrough/REST; boosted raw for weekly REST elements (skips STUDENT).
- `cmd/untisctl/main.go`, `cmd/perm/main.go` — recon|boosted|absences|writes
  grantable features + auto-revoke (XOR) + perms list display.
- `README.md` — documents the tiers.