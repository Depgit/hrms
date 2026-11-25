# Saltstayz HRMS

Go API + React frontend + MongoDB (a one-node replica set; PostgreSQL only for `hrms import-pg`), built from `Saltstayz_HRMS_Build_Spec.pdf` (the spec) with prototype v81 as the
executable reference for business logic.

## Run it

Needs Go 1.22+, Node 20+ and Docker.

```sh
make db        # PostgreSQL on localhost:5433
make migrate
make seed      # reference data + sample organisation (5 cities, 6 properties, ~53 people)
make api       # http://localhost:8080
make web       # http://localhost:5173
```

`make seed` gives two accounts a starting password, `pass1234`: **EMP-1002** (HR Admin) and **EMP-1020**
(employee). It is set only when the account has no password yet and skips the strength rules, so change it before
real use. Everyone else signs in the first time with **Activate with OTP**. In development (`OTP_DEV_ECHO=true` in
`backend/.env`) the OTP is shown on screen instead of being sent by SMS. Seeded people to try:

| Employee ID | Person | Role views |
|---|---|---|
| EMP-1002 | Sandeep Rawat | HR Admin |
| EMP-1004 | Meera Kulkarni | HRBP (Gurugram and Delhi) |
| EMP-1008 | Rakesh Bajaj | Property Manager (Sector 29 Hub) |
| EMP-1009 | Anil Kapoor | Line Manager (derived from reports) |
| EMP-1011 / 1012 | Sunita Rao / Deepak Arora | Payroll / Payroll Manager |
| EMP-1014 | sample staff | Employee only |

### Deploy (production)

Production runs on Google Cloud Run at https://people.saltstayz.com with its own MongoDB Atlas cluster, deployed by
publishing a GitHub Release (`.github/workflows/deploy.yml`). The full setup is in `docs/hrms-deployment.md` of the
loyalty-program repo. On a new installation, after the first deploy: `hrms migrate`, `hrms seed` (never `-sample`),
then `HRMS_ADMIN_PASSWORD=… hrms create-admin -name "…" -mobile 98xxxxxxxx` once. People then activate with an OTP sent
by SMS (`MSG91_AUTH_KEY` and `MSG91_SMS_FLOW_ID`).

### Google Drive for uploaded documents (one-time setup)

Every uploaded document is kept in the company's Google Drive. Until this is done, uploads wait on the server and
nothing is lost. An admin of the company's Google Workspace does this once:

1. Open [console.cloud.google.com](https://console.cloud.google.com), create a project (e.g. "Saltstayz HRMS") in
   the company's organisation.
2. **APIs & Services › Library**: enable the **Google Drive API**.
3. **OAuth consent screen** (Google Auth Platform › Branding and Audience): app name "Saltstayz HRMS", user type
   **Internal** (only the company's Google accounts can sign in, and Google does not review the app).
4. **Credentials › Create credentials › OAuth client ID**, type **Web application**. Under **Authorised redirect
   URIs** add exactly the address Masters › Company shows; for the app on `http://localhost:5173` with the API on
   8080 that is `http://localhost:5173/api/drive/callback` (add `http://localhost:8080/api/drive/callback` too if
   the API is ever opened directly). On a server, use its https address, or set `GOOGLE_REDIRECT_URL`.
5. Put the client ID and secret in `backend/.env` (`GOOGLE_CLIENT_ID=…`, `GOOGLE_CLIENT_SECRET=…`) and restart the API.
6. In the app, as HR Admin: **Masters › Company › Document storage (Google Drive)** › **Connect Google Drive**, sign in
   with the Google account that should own the files, then paste the Drive folder link (a folder that account can
   edit, in My Drive or a shared drive) and **Save folder**. Files uploaded before this stay on the server until
   **Move N existing files to Drive** is clicked.

The Google sign-in (refresh token) is stored encrypted (AES-GCM) with a key from `APP_SECRET` in `backend/.env`, or,
when that is not set, a random key created on first start in `backend/data/hrms-secret.key` (next to `STORAGE_DIR`,
mode 0600, outside the database). Back that file up with the database: without it the app asks to reconnect Drive.

### Email for the joiner's link (SMTP)

HR can send a new joiner a link to fill in their own joining details (see the decision below). The link and the
6-digit code that opens it are emailed from the company mailbox. Set in `backend/.env` (see `.env.example`):

| Setting | Meaning |
|---|---|
| `SMTP_HOST` | The mail server, e.g. `smtp.gmail.com`. Empty: nothing is emailed. |
| `SMTP_PORT` | `587` (default, STARTTLS) or `465` (TLS from the start). |
| `SMTP_USER`, `SMTP_PASS` | The mailbox sign-in (an app password, not a person's own). |
| `SMTP_FROM` | The sender, e.g. `Saltstayz HR <hr@example.com>`. Defaults to `SMTP_USER`. |
| `APP_URL` | The address people open the app at, used in emailed links. Default `http://localhost:5173`. |
| `TRUSTED_PROXIES` | The proxies in front of the API (addresses or CIDR ranges). Their `X-Forwarded-For` names the caller for the joiner page's rate limits. Default: the machine itself; `none` for no proxy. |

Sending uses Go's standard `net/smtp` (no new dependency) and never signs in over an unencrypted connection. Without
`SMTP_HOST`, HR uses **Copy link** or **Share on WhatsApp**, and gives the joiner their code on the phone from the
panel. With `OTP_DEV_ECHO=true` and no `SMTP_HOST`, emails are written to the API's log instead of being sent, and the
joiner's page shows the code on screen, like the sign-in OTP.

## Layout

```
backend/
  cmd/hrms            serve | migrate | seed
  migrations/         SQL migrations (embedded in the binary)
  internal/calc       salary, statutory, tax, attendance, settlement: pure functions, golden-value tests
  internal/approval   the one approval engine: conditions, chain building, vacant routing, SLA, seeded matrices
  internal/access     roles, rights, sensitive-field masking, data scope, sign-in rules
  internal/store      pgx pool, migrations, JSON row mapping
  internal/api        REST handlers (stdlib net/http), auth, finalizers, River background jobs
  internal/seed       reference data and the sample organisation
frontend/             React + TypeScript (Vite)
```

## Tests

`make test` runs:

- `internal/calc`: every golden-value row from spec 22.3 (Gurugram, Delhi, Mumbai; Permanent, Contractual,
  Retainer), LWF months, variable payout cycle, ESI contribution period, day status, monthly LOP, tax, gratuity,
  leave encashment, leave credit.
- `internal/approval`: the first rule that fits decides, steps in the rule's order, switched-off rules skipped,
  Approve beats Notify, joining initiator skip, exit types, same person asked once, amount bands, departed approver
  routing, the save checks, and the proof that every converted grid routes exactly as before (old and new engine
  compared over every boundary value ±1 and ±0.5, every listed value plus an outside one, four ways of naming people).
- `internal/access`: scope rules and SQL, password and ID rules, masking, rights consistent with the sensitive matrix.
- `internal/api`: end to end against PostgreSQL (needs `HRMS_TEST_DATABASE_URL`): OTP sign-in, scope, masking,
  leave approval order, own-request block, claim to ledger, departed approver skip, one holder per place, salary preview.

Manual testing: [docs/manual_test.md](docs/manual_test.md) is the tick-box checklist. It covers setup and
sign-ins, what each role sees, every page, the end-to-end flows, security, and the Set shift rules.

## Decisions taken where the spec left room

- **Go instead of Node** (owner's instruction). BullMQ is Node-only, so background jobs use
  [River](https://riverqueue.com), a Go job queue stored in PostgreSQL: no Redis to run. River creates its own tables
  during `migrate`. The maintenance sweep (master retirements, scheduled job changes, status by date, approval
  escalation) is a periodic River job every 5 minutes, scheduled only by the elected leader when several API servers
  run. `Server.Enqueue` inserts a job inside a database transaction, so later work such as payroll runs and bulk
  imports is queued only if the data change commits.
- **Bulk upload of approver names (owner, 26 Nov 2026).** The red "Approver names missing" box on Approvals ›
  Approval flow has **Bulk upload names** next to Add names (Role mapping at Edit, `acc.users` 2; also Bulk update ›
  Approvals › Approver names, `apprnames`). Download template = one row per missing name (the same list as the box,
  one row for every request type): Role, Level (Property / Department / Corporate department / Company), Place, Place
  Code, Where, Needed By (flows using the role, info), Employee (drop-down of every active employee as "Name
  (EMP-…)", the people Map a role offers). Upload saves it as Access › Role mapping does: added to the person's
  mapping of that role (selected locations or departments) or a new mapping (Selected locations for a property, All
  locations for a department or company role), with the same checks (active, overlap) and audit; blank Employee
  skipped; a slot held by someone by now, a switched-off or cluster/city/state mapping are refused (change those on
  Role mapping). Generic `bulkTemplate.fill`: the plain template carries the rows. Test `TestBulkApproverNames`.
- **"Approval required" tick per request type and flow (18 Nov 2026; meaning set 21 Nov).** On every flow in
  Approvals › Check a flow › All flows at a glance, only there (every request type, the shortlist and interview too;
  only who can edit flows, `appr.setup` 2). The Approval flow tab has no tick (rules and approvers only). Ticked (the default) = the rules decide who
  approves. **Not ticked = no approval**: every new request of that type and flow goes to nobody and is approved at
  once, with "Approved automatically: approval hierarchy is off" in its history and the usual next steps (finalize).
  E.g. untick Shortlist and Interview: a shortlist is approved when sent and the candidate goes straight to Selected,
  ready for the offer. The rules and approvers are kept for when it is ticked again. Requests raised before the tick was
  cleared and still waiting keep their approvers; HR may **Skip approval** on them with a reason
  (`POST /api/approvals/{id}/skip` `{reason}`, `canSkip` on `GET /api/approvals/{id}`; HR Admin, an HR role with
  `appr.setup` at Edit, or the HR person who raised it; "Approval skipped by <name>: <reason>", audited). Raise forms no
  longer show Skip approval (`GET /api/approvals/skip-check` always says no). Test a request shows "nobody is asked".
  Stored as `approval_flow.hierarchy_off` (absent = ticked, no migration); `PUT /api/approvals/flows/{wf}/{flow}/hierarchy`
  `{on}` → `{hierarchy, waiting}`, audited.
- **Approval flows are rule lists (owner, 11 Nov 2026).** Each request type has, per flow (Operations and Corporate
  tabs; leave per leave group), an ordered list of rules. A rule has a name, conditions that must all hold (grade,
  leave days in 30 days, requests this month and type, amount, pay change, above budget, outside the pay band, notice
  waiver, transfer type, claim type, change, exit type), its approvers in order, and who is also told. Rules are
  checked from the top and the **first rule that fits decides**; the last rule, **Otherwise** (no conditions, cannot be
  moved, deleted or switched off), decides when none fits, so every request has a route; a rule can be switched off.
  A person on two stages is asked once, a role cannot both approve and be told, and the person who raised an offer
  does not approve it again, as before. Stored in `approval_flow {wf, flow, rules, no_response, updated_by,
  updated_at}`; API `GET /api/approvals/flows`, `PUT /api/approvals/flows/{wf}/{flow}` (`{rules, noResponse}`, checked
  and audited) and `POST /api/approvals/flows/{wf}/{flow}/test` (`{facts, employeeId?}` → the rule that decides and
  the route). Until then a flow was a grid (condition rows × role columns, every matching row combined, column order
  = approval order; spec 21 #1). Migration `004_approval_rules` converted every grid automatically into rules that
  route **exactly** as the grid did (`approval.Convert`: every combination of matching rows becomes a rule, the
  largest first, then rules are dropped, joined or simplified only while every region of facts keeps its route), and
  kept the grids untouched in `approval_matrix` as the archive; `hrms import-pg` converts after copying, and every
  start converts any grid still without a rule list. `hrms seed` converts the stored grids before it adds the
  seeded lists, so seed and the migrations can run in either order after upgrading. A grid's negative "From" (the
  old editor accepted one) counts as from 0. The sheet's leave and amount ranges left gaps (half a day, 1.5 days,
  5.5 days, a claim of ₹5,000.50) where a request fitted no row and was approved by nobody; the seeded lists of
  leave, claims, advances, deductions, arrears and full and final are therefore "up to" rules checked from the top,
  with Otherwise taking the top range, and migration `005_seeded_ranges_meet` gives them to every list still exactly
  as seeded (a list HR has changed is left alone). The reason saved on a request (`why`, "Approvers added because")
  is now the name of the rule that decided plus anything added on top (moving into a corporate department, a
  CHRO rehire), since only that rule adds approvers. Rules check only what the request type carries, and the
  receiving roles take part only in a transfer (the API refuses the rest). Requests already raised keep the route
  and reason stored on them.
- **Calendar days basis** uses the real month length (spec 21 #3), not the prototype's fixed 31.
- **Passwords** use argon2id. Sessions are random tokens stored hashed, 12 hours.
- **Confidential-group view rights** follow the seeded Full/Masked/Hidden matrix so the two cannot disagree.
- **The UI is the prototype's** (`Saltstayz HRMS (1).html`, 29 Oct): `frontend/src/styles.css` is the prototype's style
  sheet unchanged, and pages use its markup and class names (`.sw` switches, `.chain` approval pills, `.ov`/`.dlg`
  dialogs, `.stmt` statements, `.cell` roster cells, `details.adv`), so a page looks the way the prototype draws it.
  Only the prototype's demo bits are left out ("Viewing as", sample data, "Simulate no response"). Reports and
  statutory file previews return the prototype's columns; the ECR upload text stays in EPFO's format.
- **Seeded role rights follow the prototype's role definitions** (only HR Admin manages work policies and masters;
  Finance, Finance Head and CHRO view employee dues and claims), with two exceptions where the spec or a control
  wins: Payroll runs payroll and the Payroll Manager approves it (the prototype lets Payroll approve its own run), and
  Finance and Finance Head do not download by default (spec 6.3). Migration 021 applies this to an existing database
  only for rights the owner never changed in Access control. All of it can be changed there.
- **One business time zone** (`APP_TZ`, default `Asia/Kolkata`): every database session and the Go process use it, so
  "today", the open month and the times shown agree whatever zone the server runs in.
- **Punch machines** can push punches (`POST /api/punches` with a device key) or be pulled: with the office server's API
  set in Attendance → Punch machine setup, a River job checks every minute and pulls when the set interval has passed,
  asking for punches since the last pull with a 10-minute overlap. A punch counts once however often it arrives.
- **Realtime India attendance portal** (`internal/realtime`, `realtime_pull.go`, 16 Nov 2026): the office machines
  report to Realtime's web portal, which has no API. With `REALTIME_URL`, `REALTIME_USER` and `REALTIME_PASS` in the
  environment (backend/.env; never stored in the database or logged), the HRMS signs in like a browser, runs the
  Daily report "All Raw Punch Report" for a day, downloads it through the report viewer's Excel export (.xls, read
  with `github.com/extrame/xls`) and stores every swipe as a punch (source "Realtime portal"; a later swipe earlier
  than the one before it is after midnight). The client requests only six pages (sign-in, Welcome, Daily report,
  report viewer, its export handler, and the employee list for Match portal codes) from a hard-coded list and never presses another button; the portal's
  "Update software Database", "Transfer Data In ERP" and "Open & Close Gate" pages are refused. Matching: the
  employee's **Attendance code** (`employee.attendance_code`, Bulk update › Employee master update and Import
  employees, last column, unique, leading zeros ignored) against the portal's EmpCode or CardNo. Only with the setting
  **Match by Employee ID number** (off by default): without a code, the employee whose Employee ID has that number
  (EMP-1050 ↔ 1050), when exactly one does. Unmatched codes are listed
  (up to 200) and never stop the rest. Attendance › Punch machine setup › **Realtime India portal**: on/off, every N
  minutes (15+, default 30; today, and yesterday until 10:00), **Pull now** for up to 31 days in the background
  (`realtime_run` job), and the last result. Settings in `company_setting.realtime_pull`; att.setup rights as the
  pull API. `GET|PUT /api/attendance/realtime`, `POST /api/attendance/realtime/pull`; command line
  `hrms realtime-pull -from 2026-11-01 -to 2026-11-13`. Tests `TestRealtimeMatch`, `TestRealtimePortalPull`,
  `internal/realtime` (parser on a made-up .xls, the client against a fake portal).
- **Match portal codes** (`realtime_match.go`, `internal/realtime/employees.go`, 16 Nov 2026): Attendance › Punch
  machine setup › **Match portal codes** fetches the portal's list of active employees (EmployeeList.aspx, its
  "Export Excel" button only, which answers with a CSV: name, father's name, EmpCode, CardNo, email, phone,
  department, designation, branch, dates of birth and joining, shift) in the background (`realtime_match` job) and
  proposes, for every HRMS employee who is not inactive and has no Attendance code, the portal employee who is the
  same person. **Sure**: the only portal employee with the same mobile (unique on both sides) and the name or a date
  agrees, or the only one with the same name (case, dots, word order and honorifics ignored) and one more thing
  agrees (date of birth or joining, department, designation, location). **Likely**: the same mobile but another
  name; the same name only; one of several with the name whose date agrees; the only similar name (spelling, a
  middle name) with the department or a date agreeing. **Check**: several fit, HR picks one. **None**: nobody. Codes
  already given are never proposed; a code proposed for two people is Check for both. Nothing applies by itself: HR
  ticks (or "Tick all Sure") and applies (`POST /api/attendance/realtime/match/apply`, each pair checked like the
  bulk column and against the fetched list, audited per employee), or downloads Sure and Likely in the Employee
  master update format for Bulk update. The portal employees who match nobody can be downloaded too. Kept in
  `company_setting.realtime_match` without the portal's phones and dates of birth (used for matching only); the
  device passwords on the page are never read. `GET|POST /api/attendance/realtime/match`,
  `GET /api/attendance/realtime/match/download` (`?what=portal`, `?levels=Sure,Likely`). Tests
  `TestRealtimeMatchNames`, `TestRealtimeMatchPlan`, `TestRealtimeMatchPortalCodes`, `internal/realtime`
  `TestParseEmployees`, `TestClientEmployeeList`.
- **Attendance alerts** (`att_alerts.go`, 16 Nov 2026): Attendance › Punch machine setup › **Attendance alerts**,
  every alert off until HR turns it on (`company_setting.att_alerts`; att.setup rights). (1) **Missing punch**: at a set
  time (09:00) yesterday's "Missing in/out punch" days → the employee, with the last day to regularize (`RegDeadline`).
  (2) **Late arrival**: today's in later than the late-mark start (the shift, or the shift worked on a Shift deviation)
  plus the policy's grace → the employee, once. (3) **Not punched in**: no punch by shift start + grace + N minutes
  (30), before the shift ends, on a working day without leave (approved or waiting), week off, holiday or
  regularization → the employee, and one list per sweep to the L1 manager. (4) **Manager digest** at a set time
  (11:00): each L1 manager's team today (present, late, not punched in, shift not started, on leave, off, manager
  marked). (5) **Payroll cut-off**: N days (5) before the payroll window ends (`cutoff`: calendar or 26–25), each
  employee's unregularized missing-punch, no-punch and short-hours absent days that would be loss of pay; once a
  month. Only active employees employed on the day whose attendance mode is not "Manager marked". Bell (notice kind
  "attendance", link My attendance or Attendance), email when SMTP is set up and the person has an address;
  `attAlertChannel` is the one place to plug SMS or WhatsApp in (nil today). **Safety**: when the Realtime portal pull
  is on and its last run failed or is older than twice its interval, missing-punch and not-punched-in alerts are held
  and the digest says absences are not listed ("punch feed stale"); people at a location where nobody has punched
  today are not called absent ("no punches at <location> yet (machine may be offline)"). Once only: each message's
  `notice.ref` key `att-alert:<type>:<date>:<employee>` is checked before sending; `company_setting.att_alerts_lease`
  keeps two servers from sweeping together. The `att_alerts` job sweeps every 5 minutes; the card shows the last
  sweep, today's counts per type and why anything was held. **Send a test to me** puts a sample in the caller's bell.
  `GET|PUT /api/attendance/alerts`, `POST /api/attendance/alerts/test`. Test `TestAttendanceAlerts`.
- **Set shift: timings by version; the work policy decides the attendance rules** (Masters › Shifts). A shift keeps
  versions by effective date (`shift_version`, migration 022); the engine uses the version in effect on each date and
  the `shift` row mirrors today's (the maintenance sweep moves a future version on when its date arrives). A version
  dated in a payroll-locked month is refused. The shift's own Attendance rules card was removed on 24 Nov 2026 (see
  below): a version keeps its timings and "ends next day" only. A day whose planned shift ends the next
  day, or whose work policy sets a maximum early in / late out punch, is built from the punches inside the shift's
  window (4 hours either side when no limit is set) when punches arrive, and again when a shift version, the policy's
  limits or a past roster cell changes; the record is stored in `attendance_day` like any other day, so every screen,
  regularization and report reads the same in/out and month calculation stays as fast as before. Not built until
  the client explains them: Consider NA, Configurable Attendance Calculation, IS Consider First Entry In Last Entry Out.
- **Shifts: punch rules moved to the work policy; split shifts (owner, 24 Nov 2026).** Masters › Shifts has no Punch
  rules or Single punch rules card any more: the server refuses those rules (startFrom, endFrom, addMissingOut,
  forcedOut, earlyInMax, lateOutMax, officeHours, shiftIn, singlePunch) on new versions and the engine ignores them on
  old ones (no live version had one). The work policy decides: Attendance › Punching has **Maximum early in punch
  allowed** and **Maximum late out punch allowed** (hours and minutes, 0 to 12 h; 0 = no limit, the old behaviour;
  `att.earlyInMax` / `att.lateOutMax`, decimal hours); saving a change rebuilds the punch-machine days from its
  "applies from" date (never in a payroll-locked month). "If out punch is missing" and "A day with a missing punch
  counts as" (its Half day option now reads **Half day until regularized**, same behaviour) are the policy's alone.
  A shift may have a **second part** (Second part start / end, `shift(_version).start2_time` / `end2_time`, no
  migration): 06:00–10:00 + 15:00–19:00. Both parts are on one day (a split shift that ends the next day is refused),
  the second starts after the first ends. Hours are in to out less the part of it inside the gap, so the gap never
  counts (the engine has the first in and last out only: `attendance_day` keeps one pair, and MIS uploads and
  regularizations give one pair too); the day's status follows the hour rules (one 4 h part is a half day on an 8 h
  policy); the late mark is measured from the first part's start; a missing out auto-closes at the second part's end;
  early exit and the rest gap use the second part's end. Shift list, Set shift, roster cells and legend, the shift
  pickers and the Master update workbook (Second part start / end; "None" removes it) show both parts. Payroll is
  unaffected. The **Night shift** card (night hours, night shift allowance, women at night) is gone from Working time
  and the allowance from payroll and the full register (it was ₹0 in every live policy); "Night shift counts on"
  stays. Tests `TestSplitShift*` (attend, api), `TestShiftPunchWindows`.
- **Shifts: Attendance rules card removed; a shift applies to Department + Location rows, with bulk upload (owner, 24
  Nov 2026).** Set shift has no Attendance rules card: Max hours, Attendance based on, the Absent / Half day / Full day
  rules, the weekly and monthly adjustments and the shortfall inclusions are gone, so **those features no longer exist
  in the product** (the adjustments, "attendance based on" and the max-hours cap existed only on shifts). The server
  refuses them (400, unknown field) and the engine ignores them on old versions (no live version had one set); every
  day follows the work policy's full-day and half-day hours. A shift keeps its timings, second part, next day and break.
  **Where it applies** replaces "Locations: All / Selected locations only": a list of Department + Location rows
  (`shift_map`, migration 029), either side All ("Front Office at Gurgaon office", "All departments at Noida", "All
  departments at all locations"). A shift is offered to, and valid for, an employee only when a switched-on row matches
  their department and location: roster codes and the cell picker, the roster upload, the Onboarding and joining
  verification shift pickers, an employee's job and Bulk update, Import employees, transfers (a shift that no longer
  applies moves to the first that does), Called to work and Master update. On Set shift each row is added, switched on
  or off, or removed at once (no effective date), audited and shown under "Applies to: history" (`config_history`,
  entity shift); the same pair twice is refused (409) and the last switched-on row cannot go (make the shift inactive
  instead). Add shift needs at least one row; a shift added through the masters API (`POST /api/masters/shift`) or the Master update
  workbook gets "All departments at all locations". Migration 029 changed no one's shifts: All locations became All departments at all
  locations, each selected location All departments at that location. People already on a shift their rows no longer
  cover keep it (their default shift and planned roster cells do not change, the roster warns); a new assignment is
  refused with "Shift N does not apply to <department> at <location>". **Masters › Shifts › Bulk upload** (and Bulk
  update › Shift mapping, `shiftmap`): Shift Code, Department, Location (blank or All = all), Active (Yes / No, blank
  Yes), with current-data download. The Master update Shifts sheet shows the rows read-only in place of Locations.
  `POST /api/shifts/{id}/maps`, `PUT|DELETE /api/shifts/{id}/maps/{mapId}`. Tests `TestShiftMapping`,
  `TestBulkShiftMapping`, `TestShiftMapsFromLocations`, `TestShiftRunsFor`, `TestEmptyRulesAreToday`.
- **Work policies clean-up (owner, 8 Nov 2026).** No overtime, comp-off credit or pay for holiday or week-off work
  (comp-off already credited stays usable until used or expired; published and locked months are unchanged); no
  break is taken off and there is no late half day; extra hours, short rest and work on an off day are not
  deviations (rest is checked on the roster only). A person matching several policies gets the first of Individual >
  Location > Cluster > City > State > Department > Designation > Grade > Employment type. Probation can be set per
  grade. The discipline score starts pro-rata for a part month, and a policy can refuse leave below a score (HR Admin,
  or the HRBP in scope, can allow one request with a reason, audited). Removed settings in stored policies are ignored.
- **Master update and Edit on every master (owner, 9 Nov 2026).** Masters › Master update downloads one Excel workbook
  (with the current data, or blank) with a sheet per master the person can change, in dependency order: Regions,
  States, Cities, Clusters, Locations (Masters: organisation), Departments, Grades, Designations, Shifts,
  Qualifications, Employment types, Attendance modes, Change reasons (Masters: job). Each row is Name, the master's
  fields (parents by name, "Name (Parent)" where a name repeats), Status, Inactive from, Inactive reason and the Row ID.
  It is uploaded once (`POST /api/master-update/check`, then `/apply`): sheets are read in that order, every row goes
  through the same server rules as Add and Edit (`saveMaster` in `master_rules.go`), valid rows apply and rows with
  errors are skipped; a missing sheet or row changes nothing. Budgets stay in Bulk update › Budgets and shift rules
  in Set shift. Every list row has Edit (Shifts keep Set shift). Masters held elsewhere by name are renamed
  everywhere they are current in the same transaction: a grade on employees, offers, pay bands, designations, work
  policy mappings, probation by grade and approval rules; a department in work policy mappings, role mappings (their
  department scope), onboarding documents, company policy audiences, offers and pending transfers; a designation
  in policy mappings (kept for another department's designation of the same name), offers and pending transfers;
  an attendance mode on employees and offers. History and the audit log keep the old text. An employment type's
  name and a shift's code cannot change; a designation cannot move to another department while active employees
  hold it. No migration.

- **The names the app writes itself can be renamed (owner, 11 Nov 2026).** The change reasons Transfer, Excess
  payment recovery, Absent without notice, the six separation reasons and the attendance mode Biometric have a fixed
  key (`simple_master.sys_key`, migration 030, list in `store.SysKeys`). The app finds them by key and writes their
  current name, so HR renames them in Masters like any other item; what they are used for cannot change.

- **An exit HR starts is hidden from the employee until approved (owner, 11 Nov 2026).** A termination, absconding,
  contract end or retirement case is not shown to the person it is about while it waits (My requests and its count,
  approvals queues, the request, the separations list, their Separation page, Dashboard activity); a rejected or
  cancelled one never is. Their own resignation shows as before (`hrExitHidden` in `myrequests.go`).

- **Uploaded documents are kept in the company's Google Drive (owner, 11 Nov 2026).** An HR Admin connects a Google
  account (OAuth web flow with offline access, a signed short-lived state plus a browser cookie against CSRF) and
  pastes a Drive folder link in Masters › Company. Scopes: the full `drive` scope (needed to write into a folder the
  admin pasted; `drive.file` cannot see it) plus `openid email` for "Connected as". Every upload is staged on the
  server as before, then a River job (retries with backoff, plus a sweep every minute) uploads it with
  `supportsAllDrives` (shared drives work), checks the MD5 Drive reports, records the Drive file id on `document`,
  and only then deletes the staged copy. Folder per person: `SI-0001/SI-0001 Aadhaar card.pdf` (the ID, then the
  document's label from Onboarding setup); an employee who came through a candidate keeps the SI folder; people hired
  before onboarding stages use their Employee ID (`EMP-1020/EMP-1020 PAN card.pdf`); a legacy offer without an
  employee uses `OFR-1001`; the payslip logo and company policy files go to `Company`. A replaced file is renamed
  "(replaced 11 Nov 2026)"; nothing is deleted in Drive. Opening a document streams it from Drive through the app
  after the same access checks; while Drive is disconnected the app says "Reconnect Google Drive to open this file".
  When Drive is down or not connected, uploads still work and wait on the server. Files from before the change stay
  on the server until "Move existing files to Drive". Migration 029 (additive). Plain HTTPS to Google's REST API, no
  new Go module; `GOOGLE_BASE_URL` points it at a fake for tests.

- **Onboarding in stages, one number per candidate (owner, 11 Nov 2026).** Offer approval no longer creates the
  employee, so nobody who never joins uses up an Employee ID. Every new hire starts as a shortlist: HR enters personal
  details, work experience, existing CTC, location, the role and the hiring manager (optional résumé) and gets
  `SI-0001` (sequence `candidate_seq`, never reused). The shortlist approval (`shortlist`, editable in Approval routes)
  is the hiring manager (the Line Manager step), then the department's Function Head. The interview is an
  approval-engine step only the hiring manager can decide, in their Approvals, with Selected / Not selected, date and
  feedback (`interview`: no admin override, no escalation, not an editable route). Selected issues `OI-0001`; HR fills
  today's offer form, prefilled, which goes through today's offer approval (pay band and budget). After acceptance
  HR marks joined (actual date, not in the future) → `JI-0001`, or not joined. The Employee ID (next in the code series,
  rejoin rules as before) is created when the last mandatory joining detail or document is in, or at joining (Onboarding
  › Form setup); creating it does what offer approval used to (pre-boarding record, pay, personal, bank, documents).
  Payroll verification is unchanged. Candidates are approval subjects (`approval_subject`, `approval_request.candidate_id`),
  so location and department scope apply; My view never shows them. Documents are kept with `owner_kind='candidate'`,
  owner `SI-0001`, at every stage. The direct "Raise offer" and the Employees › Rejoin request are gone: Rejoin opens a
  shortlist prefilled from the old record. Offers raised before (`OFR-…`) finish exactly as before, listed under the
  pipeline. Migration 028 (additive).
- **The joiner fills their own joining details (owner, 11 Nov 2026).** Onboarding › Form setup has a **Filled by**
  column (Joiner / HR) for every field and document. Defaults: fields in Personal, Identity and statutory, Bank and
  Benefits are the joiner's; Job, Location and reporting, Hiring and Compensation are HR's; every document is the
  joiner's. System fields, pay and fields that pick from the company's lists (grade, managers, shift, attendance mode,
  salary structure, job and place) are always HR's. Store migration `006_joiner_link` sets the defaults on existing rows
  (and every start fills any row an import left empty); new custom fields default by section.
  From the joining dialog (or, before the first day, **Joiner's link and joining details** on an accepted candidate) HR
  emails the link, copies it or shares it on WhatsApp (`wa.me/91<mobile>`). The joiner opens `/join/<token>` (no
  sign-in, phone-first), confirms a 6-digit code emailed to them, fills their fields, uploads their documents (photos
  from the camera or PDF, 5 MB, stored with the candidate where HR sees them), can save and come back, and submits.
  HR gets a message in the bell ("<name> submitted joining details (OI-…)"), sees the values marked **Joiner**, can
  correct any of them, fills HR's fields and saves as before; HR's save creates the Employee ID, never the joiner.
  Security: the token is 32 random bytes and only its SHA-256 is stored (`joining_link`); one live link per offer
  (a new one stops the old); 7 days; usable only while HR could edit the details (Released, Accepted, Returned by
  Payroll; candidate not closed) and read-only once submitted. Codes are hashed, 10 minutes, single use, 5 wrong tries
  lock a code, 3 locked codes lock the link, 30 seconds between codes and 5 an hour. The code opens a 60-minute session
  (kept in the browser tab, `X-Join-Session`) for that one link only; it is not a sign-in. `/api/join` returns only the
  company, the joiner's name, offer number, type, joining date and the joiner's own fields and documents; every
  unusable link gets the same answer; each address may make 300 calls per 10 minutes and 20 calls with an unknown link
  per hour. **Without email** the code cannot be emailed: HR presses **Give a code by phone**, calls the mobile on the
  offer and reads it out (shown once, 10 minutes). **Reopen for the joiner** (after a submit) takes a note and makes a
  new link (the raw token is never stored, so the old one cannot be re-sent), emailed when possible; the details
  already entered stay. Send, open, save, upload, submit, reopen, stop, phone code and a lock are in the audit log
  (field names, not Aadhaar or bank values).

- **Approvals: joining verification and bulk approval (owner, 11 Nov 2026).** In Approvals › Joining approvals a
  joining verification (`JV-…`) lists every joining document (Mandatory / Optional, uploaded or missing, file name) with
  **View**, for whoever may see the request (the rule of `GET /api/approvals/{id}`: the approver now, the person it is
  about, whoever raised it, or a role with the approvals view whose scope covers the employee), and only files of that
  request's candidate or offer. While the request waits on you (Payroll at its step, then the Payroll Manager; HR Admin
  as override) **Edit details** turns the rows into controls and **Replace** uploads a document through the normal
  upload path. A correction is checked like the joining save (`checkFormat`: Aadhaar, PAN, IFSC, account, mobile,
  e-mail, UAN, date of birth) plus mandatory details, listed choices and existing managers, shift, attendance mode and
  qualification; it is written to the offer's joining details and, since the Employee ID already exists, to the
  employee record as the joining writes it (`applyPersonal`; a corrected bank account replaces the one taken from the
  joining details), in the request's history ("Details corrected by Sunita Rao: PAN … → …", Aadhaar and account
  masked) and in the audit log with old and new values. API: `GET /api/approvals/{id}/verify-docs` (list + editable
  fields), `GET /api/approvals/{id}/verify-docs/{doc}` (file), `POST /api/approvals/{id}/verify-docs` (replace, can act
  now), `PUT /api/approvals/{id}/verify-details {data}` (can act now). Code: `internal/api/verify_edit.go`.
  On the Payroll view's **Payroll approvals** and **Joining approvals** tabs, **Download for bulk approval** gives an
  Excel file of every request of the tab waiting on you (oldest first, up to 1,000): Request ID, type, name, Employee
  ID, department, location, details, raised on, waiting on, for joining verifications one column per joining detail
  (blue, editable; Aadhaar and account masked) and Documents ("5 of 5 mandatory uploaded. Missing: …"), then Decision
  (Approve / Return list) and Comment, a "How to fill" sheet and a very hidden "Exported values" sheet. **Upload
  decisions** shows a preview (nothing changes): will approve, will return, corrections only, or skipped with the reason
  (no decision, decision not Approve or Return, unknown Request ID, listed twice, not a request of this tab, already
  decided since the download, not waiting on you any more, moved on to another stage, Return without a comment, a
  correction that is not valid, a masked number edited instead of typed in full). A detail cell that differs from what
  was downloaded is a correction (a masked cell left as it is is not); corrections are saved through the same code as
  Edit details before the row's decision, in the same transaction. **Confirm** sends the same file again; every row is
  judged again and decided through `decideTx` (the Approve and Reject buttons' code), one transaction per request, for
  your own stage only, so the next stage still follows. Ticking rows and **Approve selected (N)** uses the same apply.
  Each run is audited (approved, returned, corrected, skipped, failed). Tax proofs are not decided in bulk. API:
  `GET /api/approvals/bulk/export?group=pay|join`, `POST /api/approvals/bulk/preview?group=…` (file),
  `POST /api/approvals/bulk/apply-file?group=…` (file), `POST /api/approvals/bulk/apply {group, source, items}`; 5 MB and
  1,000 rows a file. Code: `internal/api/approval_bulk.go`; browser check `tests/e2e/verify.mjs` (`npm run verify`).

- **Salary advances: the employee asks, opening balances are loaded (owner, 12 Nov 2026).** An active permanent
  employee asks for an advance in My pay and profile › **Salary advance** (also "Request a salary advance" on My view ›
  Payroll and in New request…): amount, 1 to 12 instalments and the reason (changed 14 Nov 2026: amount and reason
  only, the approver sets the instalments; see "PF for new joiners … salary advances" below). The form shows the rules
  and the projected instalment first. The server sets the months: paid with the open payroll month, or the next one once its Deduction
  closure is closed (`payLineMonth`), recovered from the month after. Limits, in Payroll › Pay schedule and components
  (`pay.setup` at Edit): the most one can ask for is **1 × monthly gross** (a multiple, 0 = no limit) and **one advance
  open at a time** (waiting, or approved with a balance to recover; any advance counts, including one Payroll gave or
  an opening one). Stored as `company_setting.adv_max_multiple` / `adv_one_open`; empty means these defaults, so no
  migration. The request goes through the `p_adv` approval rules as before; the employee may cancel it while it waits
  (an advance Payroll or HR raised still is not theirs to cancel). Advances Payroll or HR give from Employee dues are
  unchanged and not limited. Whoever decides the **Payroll** or **Payroll Manager** step may **Change recovery** (pay
  month, instalments, first recovery month) before approving (`PUT /api/approvals/{id}/advance-terms`), written to the
  request's history and the audit log. For Payroll and Payroll Manager, Employee dues opens on **Advances** with a line
  pointing to Approvals; other roles see the page as before. **Opening advances** (given before go-live, still being
  recovered): Advances › Opening advances › Download template (Employee ID, name for reference, advance date, original
  amount, balance, monthly recovery, recovery start YYYY-MM, reference) → Upload → preview (will add, or skipped with the
  reason: unknown, inactive or non-permanent employee, balance above the original or not above 0, monthly not above 0,
  start before the open payroll month, more than 60 months, already loaded for that employee and reference, or listed
  twice) → Confirm, which checks every row again and adds each good one in its own transaction: an approved `p_adv`
  with `payload.opening`, no "Salary advance" payment line, recovery lines for the balance (the last one the
  remainder). Skip next, Recover balance and full and final treat it like any advance. Payroll (claims at Edit) or HR
  Admin; 2,000 rows, 5 MB, .xlsx; each run audited with counts and ids. API: `GET /api/me/advance`,
  `GET|PUT /api/advances/settings`, `GET /api/advances/opening/template`, `POST /api/advances/opening/preview|apply`.
  Code: `internal/api/advances.go`; tests `TestSelfAdvance`, `TestOpeningAdvances`, browser `tests/e2e/advances.mjs`
  (`npm run advances`).

- **Employee directory: the whole company, with email (owner, 12 Nov 2026).** My view › Employee directory opens on
  **Everyone**: every active employee with name, designation, department, location and the email on file
  (`employee_personal.email`, a mail link and Copy), searched on the server (name, designation, department,
  location, email; words in any order, partial words, one typo from 4 letters and two from 8) and filtered by
  department and location, 50 a page. **Departments** lists each department with its headcount, the employee's own
  first and highlighted (also one tap away from Everyone). Tapping a department opens its org chart (the earlier
  chart, now for any department: Around me / Team view and Whole department); tapping a person opens their
  department's chart centred on them; a back link returns. Tab, search, filters, page and the open chart are kept in
  the address, so the phone's back button works. Only active people; never the code, mobile, pay, birth date or ID:
  a person is named by `ref`, an HMAC of the employee ID under the server secret. Visible in My view as before (role
  Employee). API: `GET /api/me/directory?dept=<id>` (own by default), `GET /api/me/directory/people?q=&dept=&loc=&page=&size=`,
  `GET /api/me/directory/departments`. Code: `internal/api/mydirectory.go`, `frontend/src/pages/Directory.tsx`; tests
  `TestMyDirectory*`, `TestDirMatch*`, browser `tests/e2e/directory.mjs` (`npm run directory`).

- **Documents from existing employees, within 90 days (owner, 12 Nov 2026).** An active employee who did not come
  through onboarding (imported, or added directly: no offer or candidate made the record) and misses a mandatory
  document of Form setup's list (asked at offer release or joining, for their type and department) gets an upload
  window (`doc_window`). It starts the day the feature first sees them: everyone on file at the first start after
  upgrading, the import date for an HR Admin import, the sweep's day for anyone added later. Onboarding › Form setup ›
  *Documents from existing employees*: on/off and days (default on, 90; `onb_setting.doc_window_on/_days`, empty means
  the defaults). The employee uses My view › **My documents** (and a Dashboard card while open): each document as
  Mandatory or Optional and missing, waiting for HR check, verified, or sent back with HR's reason, and the days left;
  they upload, view and replace their own (not a verified one, not one Form setup gives to HR) on their own record
  (`owner_kind employee`) with the joiner's file checks, until the last day; then "Ask HR to reopen". The general
  upload refuses an employee's own Form setup document keys, so the window cannot be bypassed. HR uses Employees ›
  **Document collection**: "3 of 5", days left or "Overdue N days", status (Open, Overdue, To check, Complete), filters,
  Verify, Send back with a reason (bell and email to the employee), Extend by N days for the ticked people (a closed
  window reopens from today), Send reminder (once a day each), Upload on the employee's behalf ("Uploaded by HR") and
  Export to Excel. HR Admin and the HRBP in their scope act (`emp.directory` and `emp.govid` at Edit); roles with both at
  View read; nobody acts on their own. A job every 15 minutes (and on start) opens windows and sends the bell (and
  email when SMTP is set) when a window opens and at 30, 15, 7 and 1 days left, and tells the HRBP (else HR Admin)
  when it ends with documents missing; each once (the key is claimed in the same transaction as the message).
  Uploads, verify, send back, extend, reopen, reminders and the setting are audited with names only. Migration
  `007_doc_collection` only creates `doc_window`; `document.review_*` are new optional fields. API:
  `GET/POST /api/me/documents`, `GET /api/doc-collection?filter=all|overdue|due7|tocheck|complete`,
  `GET /api/doc-collection/export`, `GET/PUT /api/doc-collection/settings`, `GET /api/doc-collection/{id}`,
  `POST /api/doc-collection/{id}/files|review`, `POST /api/doc-collection/extend|remind`. Code:
  `internal/api/doc_collect.go`, `internal/store/schema_doccollect.go`, `frontend/src/pages/MyDocuments.tsx`,
  `DocCollection.tsx`; tests `TestDocCollect*`, browser `tests/e2e/docs.mjs` (`npm run docs`).

- **Type to find a person, everywhere (owner, 12 Nov 2026).** Every place that picks an employee, manager, approver,
  user or candidate uses one people picker (`frontend/src/components/PersonPicker.tsx`) instead of a plain dropdown:
  it looks like a dropdown showing "Name · EMP-1023" with a clear (×) button; focus or typing opens the matches (name,
  code, and designation · department · location), ↑/↓, Enter and Esc work, the best 20 show with "Keep typing to
  narrow" when there are more, and a pinned first option where the page had one ("All employees", "Keep current
  manager"). Over a list the page already has (managers, users, the attendance scope) it matches in the browser;
  otherwise it searches `GET /api/employees?q=` 200 ms after the last key. Matching is approximate and the same on
  both sides (`internal/api/peoplesearch.go`, `frontend/src/components/personMatch.ts`): every word must fit, in any
  order, a word of the name, the code (with or without "EMP-", "1023", "emp1023"), designation, department, location,
  city, or the email for roles that see personal details; exact, then start of a word, then inside a word (3+
  letters), then a small typo (1 edit for 4-7 letters, 2 for 8+, a swap counts as one); a single word may be the
  initials ("rk" → Rakesh Kumar). The server narrows with a word search inside the caller's data scope and filters;
  when that finds fewer than 20 it scores everyone in scope for typos and initials (about 25 ms for 3,000 people).
  Scope and masking are unchanged; the Employees list's search box gets the same matching, best match first. Tests
  `TestPeopleMatch`, `TestPeopleSearch`, `TestPeopleSearchScale`; browser `tests/e2e/search.mjs` (`npm run search`).

- **Uniform sizes, issue by size, refund as per policy (owner, 13 Nov 2026).** Each employee has four optional sizes
  (`uniform_size`): Shirt / T-shirt (XS–4XL), trouser waist (26–44 in, even), trouser length (28–36 in) and shoes (UK
  4–12). HR Admin keeps the lists in Employee dues › **Setup › Uniform sizes** (`company_setting.uniform.sizes`); a
  size taken off a list stays on the records that have it. The **Uniform sizes** block is on the employee record
  (Employees › open a person) and the payroll file (**Uniform** tab): read by anyone who opens them, changed by roles
  that edit employee details or manage dues (`emp.directory`, `emp.personal` or `emp.assets` at Edit; an employee asks
  HR), each change in the change history and the audit log. New joiners give them on the joining form: Form setup has
  a **Uniform** section (`uniShirt`, `uniWaist`, `uniLength`, `uniShoe`; at joining, optional, filled by the joiner,
  switchable to HR), copied to the record with the other joining details. Bulk update › **Uniform sizes** (download
  current data, upload; blank keeps), four optional columns on **Import employees** (and the JSON import), and a ninth
  group "Uniform sizes" in Employees › Download all details. Each uniform item has a **Size by** (shirt, trouser, shoes
  or no size; `inv_item.size_type`, guessed from the name); issuing uniform (Employee dues › Uniform, people picker)
  shows the employee's sizes, pre-fills each item's size (trousers as waist/length) and records it on the line (one
  line per item and size); the issue lists, the record and My view show the sizes. Employee dues › **Uniform sizes**:
  active employees by size per item and trousers by waist × length, by location and department, with the people with
  no sizes, and an Excel download (By size, Trousers, Employees, No sizes). **Refund basis** (Setup): *after the last
  deduction* (6 or 12 months; the rule so far and the default) or *after months of employment* (1–60, default 6: the
  month the employee completes them, or the month after the last deduction if later); the recovery split is
  unchanged; a change applies to uniform issued from then on (`uniform_issue.refund_rule` names the rule used). The
  refund is paid only to someone still employed in its month: leaving before it (Mark inactive, bulk separation, a
  full and final, or the 5-minute maintenance sweep for last working days from the day this was installed; people
  who had already left keep the schedule they had) cancels the scheduled refund line (void, "No uniform refund: left before
  the refund month"), the recovery stands, and the full and final shows an info line "Uniform deposit refund not paid"
  instead of paying it early (before this change it brought the refund forward). Undo inactive puts it back. The
  employee sees what is still to be recovered and the expected refund month in My view › Payroll › **My uniform**.
  Migration `008_uniform_sizes` (additive) creates `uniform_size`, adds the four Form setup fields and tags uniform
  items named shirt/T-shirt/trouser/pant/shoe; existing issues and refund lines are untouched. API:
  `GET|PUT /api/employees/{id}/uniform`, `PUT /api/inventory/size-lists`, `GET /api/inventory/sizes[/sheet]?loc=&dept=`,
  `PUT /api/inventory/setup` (`refundBasis`, `serviceMonths`), `sizeType` on items, `size` on issue lines. Code:
  `internal/api/uniform.go`, `internal/store/schema_uniform.go`, `frontend/src/components/Uniform.tsx`,
  `pages/Inventory.tsx`; tests `TestUniformSizes`, `TestUniformJoining`, `TestUniformMigration`, browser
  `tests/e2e/uniform.mjs` (`npm run uniform`).
- **Opening uniform and opening advances in Bulk update (owner, 13 Nov 2026).** Existing staff move onto the HRMS
  mid-year with uniform and advance recoveries started under the old system. The **first HRMS deduction month** is the
  open payroll month, or the next one once its Deduction closure is closed (`payLineMonth(…"deduction", openYm)`, as
  for opening advances); both templates show it in a note. An instalment before it was recovered outside the HRMS
  and gets no pay line; one in it or later becomes the usual pay line.
  **Bulk update › Opening uniform** (`openuni`): Employee ID, Issued month (`2026-09`, `Sep-2026`, `09/2026` or an
  Excel date), Uniform amount (₹) (1 to 1,00,000), Instalments (optional, 1 to 12: equal to the paisa, the remainder
  last; blank: Setup's split), Items / note (optional), Recover (Yes/No, default Yes). The instalments run from the
  issued month; the refund ("Uniform deposit refund", the full amount) follows the refund rule from the real last
  instalment month (`uniformRefundYm`). A refund month before the first HRMS month is refused ("already refunded
  before the HRMS; nothing to load"); someone inactive, or whose last working day is before the refund month, gets the
  refund line cancelled with the leaver reason of `uniformRefundCancel` (restored on reactivation or a withdrawn
  resignation), so the full and final still says "Uniform deposit refund not paid". Recover No, or staff who are not
  permanent: the issue is recorded only. Stored as a `uniform_issue` with `opening: true`, `issued_ym`, `instalments`,
  `note`, `before_hrms` (`[{ym, amount}]`), `first_ym`, no store (`store_id` null) and no stock movement; the
  Uniform card lists the months before the HRMS as "recovered before HRMS" and counts them as recovered; the exit
  clearance does not ask for it back. Refused: unknown or pre-boarding employee, month in the future, more than 24
  months back or before joining, the same employee and month already loaded (so a re-upload is safe). Who: Employee
  dues at Edit (who issues uniform with recovery) or HR Admin. Example: ₹5,000 given in September 2026 in 2
  instalments, HRMS from October 2026: September's ₹2,500 counts as recovered, ₹2,500 is deducted from October
  salary, ₹5,000 refunded in April 2027 (6 months after the last deduction).
  **Bulk update › Opening advances** (`openadv`): Employee ID, Advance amount (₹), Advance given month, Deduction
  start month, Monthly deduction (₹), Deduction closing month, Reference / reason (optional; blank:
  "Opening advance given <month>"). Already recovered = monthly × the months from the start month to before the first
  HRMS month; the balance is loaded with `addOpeningAdvance`, exactly as Employee dues › Advances › Opening advances
  does (an approved `p_adv` marked opening, recovery lines from the first HRMS month, the last the remainder; the
  payload also keeps `deductionStartYm`, `closingYm`, `recoveredBefore`, `totalInstalments`). The closing month must
  be start + ceil(amount ÷ monthly) − 1 (the error gives the month expected). Refused: nothing left to recover, a
  start before the given month, and the same employee and reference loaded already. The company's advance limits
  (one open at a time, the multiple of the monthly gross) are not applied: these advances were already given. Who: Payroll (claims at Edit) or HR Admin. The Advances
  card's own upload is unchanged and links to the Bulk template.
  **Import employees vs the later-change templates (17 Nov)**: Import employees is the complete one-time sheet
  for new people (job, personal, bank and ID details, Attendance code, opening CL/PL/SL, uniform sizes). Employee
  master update, Personal details, Uniform sizes and Opening leave balances are for later changes: each has
  "Download current data" with every employee's current values (imported ones included; Employee master update and
  Opening leave balances gained it, `bulk_current.go`), and a value uploaded unchanged is no change (no reason or
  date asked). The no-sizes list, the dashboard's missing details and the employee file read the same records the
  import writes, so imported values count as given. Each template's help says which is for what.
  **Opening leave balances**: checked that a balance "as at" a date includes that month's credit and monthly accrual
  (Days/12) resumes the month after (CL 12 a year, 8 as at 31 Oct: 9 on 15 Nov, 10 on 1 Dec; 1 Oct the same; upfront
  credit adds nothing more that year); the help text says so. Each template's "How to fill" sheet explains the
  columns with the owner's example. Bulk update's tab row is replaced by a picker grouped **Employees**, **Pay and
  dues**, **Leave and time** (wrapped, phone width without sideways scroll, `?tpl=` kept and updated); Uniform sizes
  is always offered to HR Admin. No migration: the new `uniform_issue` fields are additive and null on older issues.
  Code: `internal/api/bulk_opening.go`, `internal/store/schema_uniform_opening.go`, `pages/Bulk.tsx`; tests
  `TestBulkOpeningUniform`, `TestBulkOpeningAdvances`, `TestOpeningUniformPlanOwnersCase`, `TestOpeningAdvancePlan`,
  `internal/leave` `TestOpeningBalanceAccrual`, browser `tests/e2e/bulkopen.mjs` (`npm run bulkopen`).

- **PAN verification, job roles and skills, cards, optional holidays, import without a salary (owner, 14 Nov 2026).**
  - **PAN verification** (`b1_pan.go`): Masters › Company › **PAN verification** (HR Admin): provider None / Test /
    Surepass / Cashfree / Signzy / Karza, API key, https base URL, on/off, stored in `company_setting.pan_verify`
    with the key sealed like the Drive token (AES-256-GCM under `APP_SECRET` or the key file; only its last four
    characters are shown). **Test** works offline: a PAN whose 4th letter is P verifies when its 5th letter is the
    first letter of the surname (as real PANs are numbered), another initial is a name mismatch, anything else is
    invalid. The real providers are stubs: "not configured" without a key, and nothing sent with one; no external
    call is made in tests or development. A PAN is checked when it is saved on the joining details (HR's or the
    joiner's link; `offer.pan_check`, carried to the record with the PAN) and with **Verify PAN** on the record
    (`emp.govid` at Edit, in scope). Kept on `employee_gov_id` (`pan_status`, `pan_name`, `pan_checked_at`,
    `pan_checked_by`, `pan_checked`) with the PAN checked, so a later PAN reads "Not checked"; audited with the PAN
    masked. The PAN itself is masked as before. API `GET|PUT /api/settings/pan-verify`,
    `POST /api/employees/{id}/pan/verify`.
  - **Role and Skill masters** (`b1_masters.go`): "Role" is the employee's *job* role, never an access role: the
    master types, `simple_master` kinds and employee fields are `job_role` and `skill`. Masters › **Roles** and
    **Skills** (active/inactive, rename follows to employees and open offers) and Master update sheets Roles and
    Skills. Required on the offer like the department (Form setup `jobRole`, `jobSkill`, system fields, the active
    names as options), copied to the record; existing employees without them stay valid. Bulk update › Employee
    master update (Role, Skill: changed like the other job fields, scheduled for a future date) and Import employees
    (optional columns); on the record and in Employees › Download all details. The old free-text "Skill or trade"
    field is left as it was (HR can switch it off in Form setup).
  - **Medical** (Employee master update): the full salary register's employer "Medical insurance" came only from
    the salary structure (Medical % of gross up to the cap, for staff not under ESI); nothing was held per employee.
    So `employee_pay.medical` is the employee's monthly medical insurance (₹), an employer cost (not a deduction),
    replacing the structure's rate and cap (`withMedical`: the amount, at most the gross earned; ESI or medical,
    never both, as before). "Standard" clears it. Used on the record's CTC, Salary breakup's reference columns and
    the employee download. The payroll engine (`payengine.go`, Builder 2's) must pass it for the register to use it:
    `Medical: payengineFloatPtr(r["medical"])` when it loads `employee_pay`, and `Structure: withMedical(s, pay.Medical)`
    in its `calc.Calc` call.
  - **Accommodation** (Employee master update) sets the same `employee_pay.accommodation` as Salary breakup's
    "Accommodation Deduction" (permanent staff only). Both pay columns need the salary right and the row's reason and
    date, and are kept in the change history.
  - **ESIC and medical cards** (`b1_bulk.go`, Bulk update › Employees): a ZIP (100 MB at most, 2,000 files) of
    PDF/JPG/PNG files of 5 MB at most named `EMP-1007_ESIC.pdf` / `EMP-1007_MEDICAL.pdf` (`-` and any case too).
    Check lists matched, unmatched (no such employee or outside access) and invalid files; apply stores each through
    the document store as document type "ESIC card" (`esicCard`) or "Medical card" (`medicalCard`), added to
    `onb_document` not asked on the forms; a replaced card stays in the history. The ZIP is read in memory, never
    unpacked to disk; names with `..`, an absolute path or a drive are refused, a file packed more than 200 to 1 or
    over 5 MB unpacked is refused, and all files together at most 600 MB. Rights: whoever uploads employee documents
    (employee details at Edit with the government ID right). `POST /api/bulk/cards/{check|apply}`.
  - **Opening optional holiday balances**: optional holidays are not a leave type: the work policy gives
    a number a year (`settings.optHol`) to choose from the calendar's Optional holidays, taken as leave. So the
    opening balance is its own collection `opt_hol_opening` (employee, As at, balance 0 to 31), loaded like Opening
    leave balances (same employee and date replaces, a later date takes over); it is what is left of the As at date's
    calendar year and shows on the record and in the leave balances (`optHol`). Policies at Edit. Since 17 Nov it is
    the optional column **Optional Holiday Balance** of Bulk update › Opening leave balances (`lvopen`; the separate
    `optopen` template is gone): a row gives a leave type and balance, the optional holiday balance, or both, checked
    together before either is written; a blank cell leaves that balance as it is.
  - **Import employees without Monthly Gross**: the column is gone; the employee is created with no gross (the
    default structure mapping stays), the salary is set afterwards with Salary breakup or Compensation. An old file
    with the column is read with it ignored and a note on each row. The dashboard's Details alert now counts people
    with no gross ("… PAN, Aadhaar, bank or salary pending"); a payroll dry run shows them with zero pay and the "Zero
    or negative net" flag. The JSON import API is unchanged.
  - **Children**: Personal details template 1st Child and 2nd Child after Father or Spouse Name
    (`employee_personal.child1`, `child2`; blank keeps); Form setup Father or spouse name, 1st and 2nd child name
    (optional, joiner) around Marital status; on the record and in the download.
  - **Existing UAN?** (`employee.existing_uan`, bool, missing = unknown): from the joining form's existing "Existing
    UAN available?" (`uanAvail`), Import employees and Personal details (Yes needs a UAN; government ID right at
    Edit); shown next to UAN. No payroll rule uses it here (Builder 2's PF work does).
  - Migration `009_role_skill_cards` (additive): creates `opt_hol_opening`, inserts the five Form setup fields and the
    two card document types when missing; nothing existing is changed. The seed adds starting lists of roles and
    skills. Tests `TestB1PanVerification`, `TestB1RoleSkillMasters`, `TestB1MasterUpdateColumns`, `TestB1CardsZip`,
    `TestB1OptionalHolidayOpening`, `TestB1ImportWithoutGross`, `TestB1ChildrenAndExistingUAN`.

### Payroll fixes after the deep payroll test (13 Nov 2026)

Every FAIL from the payroll test (run, items, exit and access, real data) is fixed, along with the owner's four decisions of 13 Nov.
- **Run integrity:**
  - Approve uses the stored verified rows, and is refused while the run is stale or when the approver verified the run or edited a row in it (maker-checker).
  - Stale also covers gross, attendance, leave, regularization, policy and bank changes.
  - Nobody edits their own row. Paid-days overrides are capped at the days employed.
  - Only the open month can be run.
  - Attendance uploads, leave and regularization into an Approved or Published month are refused ("already paid, raise an arrear or recovery") (spec 15.1).
  - The cut-off cannot change after a month is approved.
- **Holds:**
  - Salary held for a new bank account goes on Salary holds and is paid as "Held salary released" once the account is verified.
  - Pay lines never land before the joining month.
- **Closure:** Payroll can mark unexplained absences, including leavers' days and contract or retainer staff, as loss of pay, with a reason and an audit entry (`POST /api/payroll/{ym}/closures/{attendance|leave}/mark-lop`). Since 14 Nov also missing punches, as a full or half day, and a salary with no bank account can be held (see "Payroll checklist: missing punches and no bank account").
- **Set-up warnings:** the checklist warns about:
  - a policy with no holiday calendar;
  - a leave plan without Loss of pay;
  - blank company PAN, TAN, PF or ESI codes.
- **Statutory:**
  - EPS and EDLI wages are capped at ₹15,000 (EPS at most ₹1,250, the rest to EPF) in the salary calculation and the ECR.
  - ESI and ECR wages are the earned gross (no reimbursements, bonus or pay lines).
  - ESI days are whole numbers, and NCP days leave out days outside employment.
  - UAN must be 12 digits and ESIC 10 or 17. PF is rounded to the rupee.
  - Every file download is audited.
- **Tax:**
  - Finalising before the proof window ends needs a reason, and a finalisation can be reopened.
  - Opening TDS import for months before go-live (`/api/tds/opening`).
  - TDS counts approved months that are not yet published.
- **Net pay:** rounded to the rupee, with a "Rounding off" line. The register, payslip, words, bank file and hold release all use the same figure, and every row adds up.
- **Increments:** a mid-month increment is split by days, and arrears follow the same split.
- **Attendance:**
  - Late inside grace counts as on time.
  - Half-day leave is 0.5 LOP.
  - Work on a weekly off or holiday never adds LOP.
- **Full and final:**
  - Leave encashment follows the leave type's "Encashment on exit".
  - PL carry-forward.
  - Gratuity on calendar years from DOJ to LWD, counting the LWD.
  - The pay month falls within LWD + 45 days.
  - A negative settlement becomes a receivable, which blocks the no-dues certificate until it is recovered or waived.
  - Excess salary paid after a back-dated LWD is recovered.
  - No double recovery.
  - Absconder LWD, "serve" notice from the approval date, and the rehire "Not eligible" check.
  - Amounts are hidden from managers and HRBP.
- **Masking:** a masked salary shows no digits.
- **Data on restart:** no migration. New optional fields only, on these collections:
  - gross_history: from_date
  - employee_pay: var_from, var_prev_pct
  - fnf: receivable
  - tds_history: source
  - tax_window: reopened_*
  - pay rows: roundOff, outsideP
- **Tests:** `internal/api/payfix_[a-f]_test.go`, plus new calc, attend and leave tests.

### Fixes after the full test, other areas (13 Nov 2026)

The 105 FAILs outside payroll were fixed, found already fixed, or left for a decision (`TIME-187` early-out grace,
`TIME-074` future "Applies from"). What changed:
- **Sign-in:**
  - Setting the password needs the one-time `verifyToken` that `/api/auth/otp/verify` now returns, so knowing someone's
    Employee ID after they verified is not enough. Scripts that sign in must pass it (Go tests, e2e and parity do).
  - From one address, 10 failed sign-ins (wrong password, unknown ID or wrong OTP) in 15 minutes give 429 until the
    window passes. OTPs: 10 per address per 15 minutes and 10 per Employee ID a day. With `OTP_DEV_ECHO` (development
    and test scripts only) the two per-address limits are 300.
  - Sign-in, wrong password, account lock and sign-out are in the audit log (with the address, no secrets).
  - EMP1002, emp1002 and EMP 1002 sign in as EMP-1002. A session of someone moved back to Pre-boarding ends.
- **Masking:**
  - A bank change request's account number and IFSC follow the viewer's bank group in the approval detail, every
    approval queue and the employee file.
  - Someone else's change history leaves out personal rows without the personal details right, masks pay rows when
    salary is Masked and leaves them out when it is Hidden.
  - PAN numbers in request history notes are masked or hidden unless the viewer sees government IDs in full.
  - Payslips viewed by anyone but the employee mask PAN, UAN and bank by the viewer's own groups, payroll roles too
    (a CHRO sees none); the design preview is masked the same way. `GET /api/payslip-setup` needs a payroll right.
  - Joining verification documents go to whoever acts on the request, the person, the sender, or a role with the
    government ID right; a candidate's ID card copies (Aadhaar, PAN, address proof, Form 11, bank proof) need that
    right or an onboarding right. HR's joining save and Payroll's corrections log Aadhaar and account masked.
  - The employee export writes personal details in full (the section needs the personal right) and writes any text
    starting with = + - @ as text.
  - Location and city masters leave out budgets for roles without the Budgets or org masters right.
- **Approvals:**
  - A route drops a stage only when it is the same employee, so two people with the same name are both asked.
  - Bulk approval is refused in My view; a request the caller cannot open reads "Unknown Request ID".
  - My view shows Approvals while a request waits on the person by name (a Receiving Manager with no role view).
  - Someone who has left raises no requests of their own (spec 6.4), except a claim within 15 days of the last day
    (spec 15.2). "Manager can regularize for the team" is
    enforced. With regularizations counted together, both kinds count for the route.
  - A night-shift regularization may have its out time the next morning.
  - "Reporting manager has left" moves only the reports of a manager who has left.
  - Releasing, accepting or declining an offer needs an onboarding right (not Publish pay changes).
- **Separation:** on notice the employee's withdrawal goes through the Withdraw resignation request (HR can still
  withdraw directly, which also cancels unpaid notice recovery); a withdrawal cancels other waiting change or
  withdrawal requests; an employee's own resignation ignores a waiver; a new last working day is at most a year away;
  clearance is signed only for a leaver in the signer's scope (or by the reporting manager).
- **Employees and masters:**
  - Job change: a grade alone must be on the current designation; no reporting loop (L1 or L2).
  - Personal details: a mobile already on a current employee is refused; address 500, names 100, email 120
    characters; an email differing only in case is no change.
  - Import employees: the personal details rules (gender list, date of birth past and 14+), Aadhaar and mobile not
    already on file, Inactive only with a past last working day, name up to 100 characters and not starting with
    = + - @. Codes and code prefixes no longer allow "/". The JSON import's preview finds a manager defined earlier
    in the request, as applying does.
  - Excel uploads read a date cell as its date whatever its display format; text dates are day first, also with a
    two-digit year (12-06-24) or a month name (13-Nov-2026).
  - Bulk update round trips: a Salary breakup value equal to the current one is no change; a city and grade with
    no band left blank is no change; two cities with the same name are written "Name (State)" in Pay bands and
    Budgets; a fractional headcount is an error on its row only.
  - Employment types are fixed to Permanent, Retainer and Contractual (no Add); the built-in types and the items the
    app writes itself (Resigned, Biometric and the rest) cannot be made inactive. A custom role can be mapped to a
    person; copying from a role that does not exist is refused; rights changes keep "before" in the audit log.
- **Compensation and tax:** every structure rate is checked (no negatives, percentages ≤ 100, ESI ceiling above 0);
  version names are unique ignoring case (40 characters); the calculator refuses negative deductions; a band needs an
  active city and grade; a band copy factor is above 0 and at most 5, and the copy keeps the overwritten bands in the
  audit log; Labour welfare fund shows the rule in force and notes a scheduled one, months 1-12 and percentages ≤ 100;
  CIN format checked. Tax windows sit near their year; the same proof document cannot be sent twice; "Remind" puts a
  notice under each person's bell; an item limit cannot go above the law; part-year PF in 80C is month by month.
- **Time:** shift codes WO, PH, L and LOP are reserved; work policy choices and ranges are checked; a mapping must
  name a real record; a date is a holiday once per calendar; machine punches repeated within 2 minutes are ignored;
  a complete uploaded day is not replaced by an incomplete machine day; the roster's rest gap uses "Consider shift
  in"; an unknown punch machine is 404.
- **Data on restart:** no migration. One new optional field, `auth_otp.verify_hash`; an OTP verified before the
  upgrade has none, so that person verifies again.
- **Tests:** `internal/api/ftfix_test.go`, `internal/api/ftfix_unit_test.go`, `internal/attend/ftfix_test.go`.

### Payroll checklist: missing punches and no bank account (owner, 14 Nov 2026)

Some checklist items never clear without the employee. Two more actions on Payroll › Payroll run › "Before you run
payroll", with the rights and timing of Close and Mark as LOP (Payroll's run rights at Edit, so Payroll, the Payroll
Manager and HR Admin; the open month only, while it is Open or in Dry run and the closure is not closed; a reason of
at least 5 characters; audited).
- **Attendance: Mark as LOP for a missing punch** ("Missing punch: in 09:00, no out time"), Full day or Half day.
  Absences stay a full day. The day becomes an approved Loss of pay leave raised by Payroll, as Mark as LOP did for
  absences (`markLop`). A half day is stored exactly as the leave module stores a half-day leave: one date,
  `payload.half: true`, `facts.days: 0.5`, so the attendance engine counts 0.5 loss of pay and the punched half as
  worked (half-day leave = 0.5 LOP), and the calendar, the leave list, the attendance summary report and payroll all
  read it from the same request. **Mark all as loss of pay (n)** (it keeps its old name "Mark all absences as loss of
  pay" when only absences are listed) asks one Full day / Half day choice for the missing punches. API:
  `POST /api/payroll/{ym}/closures/{attendance|leave}/mark-lop {refs | all, reason, half}`; the answer counts
  `absences`, `fullDays`, `halfDays` and `lopDays`.
- **Bank verification: Hold salary** for "No bank account on record", and **Hold all (n)**; the reason is prefilled
  "No bank account on record". It is the salary hold of Salary holds (`placeHold`, `salary_hold`, reason "No bank
  account on record", `source_id: "no-bank"`) for the open month only (`to_ym`). The salary is calculated but held:
  left out of the bank file and the net paid, listed on Salary holds, the employee file and My view; the payslip is
  issued as for any hold. The item leaves the pending count and shows under **On hold (n)** (`held`, `onHold` on the
  closure), so the closure can close. Still without an account next month, the person is listed again; Hold salary
  then moves the same hold on (`to_ym`, `extended_at`). Once an account is on record for the open month (added by
  import or Bulk update, or a bank change verified) and no bank change waits, the hold is released by System, like
  the bank-change hold (`releaseNoBankHolds`, called by `syncBankHolds` and before every payroll calculation): each
  held month already run is paid in the open month as "Held salary released, <month>". Payroll can still release it
  by hand. API: `POST /api/payroll/{ym}/closures/bank/hold {refs | all, reason}`.
- **Data on restart:** no migration. Two new optional fields on `salary_hold`: `to_ym` (the last month a hold holds;
  null on every existing hold and on holds placed in Salary holds, which hold every month from `from_ym` as before)
  and `extended_at`. Loss-of-pay requests may carry `payload.half`, as half-day leave already did. No new GET route.
- **Parity:** the checklist gains `held`, `onHold`, and `lopHalf`, `canHold` and `holdId` on items; Salary holds and
  My view's pay tab gain `to_ym`; marking a missing punch is now offered where the Postgres build had only Send
  message.
- **Tests:** `internal/api/closefix_test.go` (`TestCloseFixMissingPunchLop`, `TestCloseFixMarkAllMix`,
  `TestCloseFixBankHold`, `TestCloseFixBankHoldReleasedByRun`); browser steps in `tests/e2e/forms.mjs` (a joiner with no
  bank account and a missing punch: Half day, Mark all, Hold salary, On hold).

### ICICI bank upload and the full salary register (owner, 14 Nov 2026)

Two payroll downloads next to the bank advice and the register, which are unchanged. Both need the register's rule
(Export payroll files at Edit and the role's download right), cover the caller's data scope and are audited
("Downloaded <file>"). Code: `internal/api/bankfile.go`; tests `TestBankFileICICIAndFullRegister`, `TestICICI*`.
- **ICICI bank upload** (`GET /api/payroll/{ym}/icici-upload.xlsx`, `ICICI-bank-upload-Oct-2026.xlsx`): only for an
  Approved or Published month. One sheet with ICICI's 13 columns PYMT_PROD_TYPE_CODE (`PAB_VENDOR`), PYMT_MODE (`FT`
  when the IFSC starts with `ICIC`, else `NEFT`), DEBIT_ACC_NO (the setting below), BNF_NAME (letters and spaces
  only: dots, hyphens, commas, slashes and underscores part words, other characters are dropped; single spaces; 500
  at most), BENE_ACC_NO (digits, 1 to 32), BENE_IFSC (upper case, `^[A-Z]{4}0[A-Z0-9]{6}$`), AMOUNT (the net pay of
  the register and bank advice, `0.00`), CREDIT_NARR (`Oct 26 Salary`), PYMT_DATE (today in IST, `DD-MM-YYYY`),
  MOBILE_NUM (the last 10 digits once +91, spaces and dashes are taken off; blank otherwise), EMAIL_ID (the personal
  email on file: the app keeps no official email), REMARK (blank) and REF_NO (the Employee ID). Every column but
  AMOUNT is written as text. The rows are exactly the bank advice's people (not on hold, net above 0; a held salary
  released this month is part of the net, so it is paid). Anyone with no account, an account number that is not 1
  to 32 digits, a missing or invalid IFSC, or no letters in the name is left out and listed with the reason on a
  second sheet **Not included** (ICICI reads only the first) and by `GET /api/payroll/{ym}/icici-upload`, which the
  **Bank files** card of Payroll run and Reports › Bank advice show before the download.
- **ICICI debit account**: Payroll › Pay schedule and components › **Bank upload** (`company_setting.icici_debit_acc`,
  `GET|PUT /api/payroll/schedule` `iciciDebitAcc`), Payroll settings at Edit, 9 to 18 digits, audited as "Bank upload
  debit account updated". Empty means the default `38605005062`, the number the owner gave: it has 11 digits where
  ICICI's template describes 12, so it is kept as given and can be changed there.
- **Full salary register** (`GET /api/payroll/{ym}/full-register.xlsx`, `full-salary-register-Oct-2026.xlsx`): from the
  stored rows of a Dry run, Verified, Approved or Published month; before approval the name ends `-DRAFT` and row 1
  reads "DRAFT: October 2026 not yet approved; figures may change". Columns: the employee (ID, name, designation,
  department, grade, type, city, location, joining and last working day, PAN, UAN, ESIC, bank, account, IFSC), days
  (in month, paid, LOP, outside employment), monthly gross and CTC a month (the engine on the full gross, as on the
  employee record), each earning (Basic, HRA, Special allowance, Bonus, Variable pay, Arrears,
  Reimbursements, Salary advance, Uniform refund, Held salary released, Other earnings: other pay lines plus
  Payroll's adjustment), each deduction (PF, ESI, LWF, TDS, Meal, Accommodation, Advance, Uniform and Asset
  recovery, Other recoveries: other lines plus Payroll's adjustment), totals, rounding off and net (the register's
  figures, so each row adds up), the employer's side (EPF, EPS, EDLI, EPFO admin, ESI, LWF, medical insurance,
  gratuity provision, total, cost to company) and On hold, Hold reason and Remarks; a Total row; header and the first
  two columns frozen. IDs, bank details and amounts follow the caller's Full / Masked / Hidden (own row in full);
  totals of amounts are shown only to a caller who sees salaries in full. On Payroll run (**Download full register**,
  and on each approved month of the register card) and Reports › Payroll register.
- **Data on restart:** no migration; one new optional field, `company_setting.icici_debit_acc` (null until saved).

### PF choice for high earners without a UAN, UAN number, one medical plan (owner, 19 Nov 2026)

- **Threshold ₹25,000.** `company_setting.pf_exempt_gross` now defaults to ₹25,000 (was ₹20,000); a value the company
  saved in Payroll settings wins. The comparison stays gross > threshold.
- **"Do you want PF deducted from your salary?"** (Form setup `pfChoice`, Identity and statutory, Yes/No, joiner)
  is asked on the joiner's link and HR's joining form only when Existing UAN? is **No** and the offer's monthly gross is
  above the threshold (Permanent / statutory types), with one plain line on what each answer means; then required
  (also checked at **Send to Payroll** and Payroll's corrections). **Yes** = `employee_pay.pf_choice: "member"`: a PF
  member on the normal structure, exactly as before. **No** = `"optout"`: PF exempt (the rule gives "Chose no PF
  deduction … paid as all Basic"), and `employee_pay.all_basic` from `all_basic_from` (the joining month or the open
  payroll month): the whole fixed gross is **Basic**, HRA 0, special allowance 0 (`calc.Input.AllBasic`; a structure
  made from components shows one Basic line, its deduction and variable lines stay), no employee or employer PF.
  Variable target, ESI, LWF and bonus rules are unchanged; gratuity and PL encashment follow the (larger) Basic. The TDS
  projection (payroll TDS, tax declaration, Form 16) uses the same split: no HRA exemption under the old regime, no
  employee PF (`taxFigureExtra` allBasic; test `TestTaxAllBasicNoHraExemption`).
  The engine honours `all_basic` only while the person is PF exempt. Recorded at joining (`applyOfferExtras` →
  `applyJoinPfChoice`, by "Joining form", audited); no choice keeps the 14 Nov rule unchanged. The offer breakup
  (`offerPay`) shows the all-Basic version once the choice is No.
- **PF card**: shows the choice, who/when/why and whether all Basic is in force; **Change PF choice** (Payroll at Edit
  or HR Admin, reason ≥ 5 characters, audited "PF choice changed", stale run) applies from the open payroll month;
  refused when there is a UAN, Existing UAN? is not No, the gross is not above the threshold, or (to No) the person was
  already paid PF. `PUT /api/employees/{id}/pf-choice {choice: member | optout, reason}`; `GET …/pf-exempt` has `choice`.
- **PF on the offer (owner, 26 Nov 2026; `api/pf_offer.go`).** The offer form (normal and offer at joining) asks
  **"Does the employee want PF?"** (`offer.data.pfWant` Yes/No, required, no default; hidden and ignored for a Retainer
  or any type without PF). **No** with gross above `pf_exempt_gross`: the breakup and letter are the whole gross as
  Basic with no PF (employee or employer); at Mark joined the employee gets `pf_choice` optout, `all_basic`, PF exempt
  (`pf_choice_by` "Offer"). **No** with gross up to the threshold: check, raise and release refused with "Gross is up to
  ₹<limit>: an employee who does not want PF must be a Retainer. Change the employment type to Retainer." **Yes**: the
  normal structure (both PF shares from the salary under the all-inclusive gross). The answer is copied to
  `data.pfChoice`, so the joining form asks again whenever the offer said No (and no existing UAN), prefilled; the
  joining form's answer wins (`pf_choice_by` "Joining form"). A joiner's No on a gross up to the threshold is recorded,
  not blocked: bell to HR Admin and Payroll at Edit, red line on the PF card (PF still applies); HR can set them back
  to PF there. Timeline/audit: "PF choice on the offer" / "… changed on the offer", "PF choice (changed) on the
  joining form / joining details", employee "PF choice recorded" / "PF choice changed". No migration.
- **UAN number**: the joining form's UAN field is named "UAN number"; shown and required (12 digits) only when
  Existing UAN available? = Yes; saved as `employee_gov_id.uan` (what the UAN hold checks). Bulk update › Personal
  details has **UAN** (12 digits, government ID right at Edit, change history masked) before Existing UAN?, and a UAN
  in the same row satisfies "Existing UAN? Yes".
- **Medical insurance: one plan** (Employee + spouse + up to 2 children). The "Medical insurance nomination" drop-down
  is off the forms; every joiner sent to Payroll gets `data.medical` = that plan (older values left). **Spouse name**
  (Form setup `spouse`, `employee_personal.spouse`) is required when Marital status = Married and hidden otherwise;
  1st and 2nd child stay optional. Personal details bulk has **Spouse Name**. Reports › New joiners lists the spouse
  and children as Medical members when the person has no medical nomination. (The ESIC and medical cards ZIP only
  stores card files by Employee ID; it reads no family data.)
- Migration `021_pf_choice`: inserts the Form setup rows `pfChoice` and `spouse`, renames the UAN field (while still
  "UAN"), switches the medical drop-down off; no pay data is touched. `hrms pay-compare -month 2026-10,2026-11`
  `-dump` before / `-against` after on a copy of the live data: 0 differences. Code: `api/pfuan.go`,
  `api/onboarding.go`, `api/joining_link.go`, `calc/components.go`, `store/schema_pfchoice.go`, `PfExempt.tsx`.
  Tests `TestAllBasicOptOut`, `TestPfChoiceOptOut`, `TestJoinFormAskedUnit`.

### PF for new joiners, the UAN hold, incentives, variable pay cycle and salary advances (owner, 14 Nov 2026)

- **PF exemption ("Gross > ₹20,000 and no UAN").** A joiner whose monthly gross is above the threshold
  (`company_setting.pf_exempt_gross`, default ₹20,000; Payroll › Pay schedule and components › **PF for new joiners**,
  Payroll settings at Edit, audited "PF settings updated") and whose `existing_uan` is false is **PF exempt**: no
  employee or employer PF, EPS, EDLI or EPFO admin (`calc.Input.PfExempt`, `Result.pfExempt` on the row), left out of
  the ECR (no PF) and the PF enrolment file, and not listed as missing a UAN. `existing_uan` is the joining form's
  field (the other builder's; read from the employee, or the government IDs, as true/false or Yes/No; missing =
  unknown = not exempt). Anyone with `existing_uan` true or a valid UAN on record is never exempt; a non-statutory
  type never is. The flag is stored on `employee_pay` (`pf_exempt`, `pf_exempt_reason`, `pf_exempt_at`,
  `pf_exempt_by`, `pf_exempt_manual`) and decided by `syncPfExempt`: every 5 minutes in the maintenance sweep (people
  whose `existing_uan` is answered, and those exempt automatically) and before every payroll calculation for the
  people in the run, so a joiner is decided at joining or when the salary is set, and again whenever gross or
  `existing_uan` changes, **as long as the person has no PF history** (no payroll row with PF in an earlier month).
  Employees already paid PF are never changed after the fact; existing employees get the rule only when their
  `existing_uan` is set while they have no PF history. Each automatic change is audited as System ("PF exemption
  decided: exempt" / "PF exemption removed"). Shown on the employee's pay section (**PF**: status, reason, who and
  when, the UAN answer, the rule's answer); Payroll (run rights at Edit) or HR Admin can **Change PF exemption**
  (PF exempt / PF member by hand, or back to the rule) with a reason of at least 5 characters, audited "PF exemption
  changed by hand"; set by hand, the sweep leaves it alone. Changes mark a calculated run stale. **Legal warning:**
  exempt with Basic ₹15,000 or less shows "PF exempt with Basic ₹… (₹15,000 or less): the law may still require PF"
  on the employee and "PF exempt with Basic ≤ ₹15,000: check <names>" in the checklist's set-up warnings; never a
  block. API: `GET|PUT /api/payroll/pf-settings` (`{exemptGross?, uanHold?}`), `GET|PUT /api/employees/{id}/pf-exempt`
  (`{mode: exempt | member | auto, reason}`). Code: `internal/api/pfuan.go`.
- **UAN hold.** In a payroll calculation (`computeRows`), everyone in the run (or the pilot) who pays PF (PF above 0,
  not exempt) with no valid UAN on record and no hold yet has the salary held from the month with the Salary holds
  hold (`placeHold`, reason "UAN not provided", `source_id: "no-uan"`, by System): calculated, left out of the bank
  file and net paid, listed on Salary holds, the employee file and My view. Joiners in the month get the same rule.
  Before each calculation `releaseUanHolds` releases such a hold once a UAN is on record, the person is exempt, or the
  check is off, paying each held month already run in the open month as "Held salary released, <month>"
  (`releaseHold`, as `syncBankHolds` does). Switchable: `company_setting.uan_hold` (default on). The **Bank
  verification** closure lists them under **PF / UAN: UAN not provided** (`uan`, `uanN` on the closure; not pending,
  so the closure still closes).
- **Incentives (Bulk update › Incentives, `incentive`).** Employee ID, Month (2026-11, Nov-2026, 11/2026 or an Excel
  date), Amount (₹), Remark (optional). Each row adds an earning pay line **Incentive** (reason = the remark,
  `source_id: "BULK-INC"`) in that month: payslip, payroll sheet additions, register details, the full salary
  register's new **Incentive** column (after Variable pay) and Upcoming pay lines. The month must be the open month or
  later (at most 12 ahead): a paid month is refused as closed, and the open month as locked once its Reimbursement or
  Deduction closure is closed or the run is verified (the row names the first open month). Also refused: pre-boarding,
  before the joining month, after the last working day, amount not above 0 or over ₹10,00,000, and the **same
  employee, month and remark** (already loaded, or twice in the file), so a re-upload is safe. Check step as the other
  templates; applying is audited ("Incentive added"). Who: Payroll (claims at Edit) or HR Admin. **Statutory choice:**
  taxable salary (in the row's total earnings that Form 16 and the tax pages count, and the month's TDS projection
  adds the financial year's incentives, `incentivesInYear` in `tds.go`); **not PF or ESI wages**, like bonus,
  variable pay and every other pay line (spec 14: "Variable is not in PF wage and currently not in ESI wage"; the spec
  says nothing about incentives). Code: `internal/api/bulk_incentive.go`.
- **Variable pay on each person's own cycle.** The payout month is counted from the employee's joining month, as spec
  14 already had it (`((m − joinMonth) mod cycle) == cycle − 1`): quarterly, joined May 2026 → July and October 2026,
  January 2027… A mid-month joiner counts the joining month as month 1; the amount is unchanged (each month's accrual
  pro rata by paid days, the joining month by days). **Existing employees:** the same rule from the joining date; when
  variable pay starts later (the % goes from nothing to something by Edit pay options or the Salary breakup upload),
  the cycle counts from that month (`employee_pay.var_start`; for a change saved before, `var_from` when the % before
  it was nothing). The employee's pay section shows "Own cycle from … : next payout …". Code: `internal/api/varpay.go`,
  `varAnchorYm` in `payengine.go`.
- **Salary advances (owner's decision: the manager sets the instalments).** Only **Permanent** employees with at least
  **3 months' service** (`company_setting.adv_min_months`) may ask; My pay › Salary advance and the API (and the
  simulator) refuse others with the reason ("permanent employees only (you are Contractual)", "after 3 months'
  service, from <date>"). The form has the amount and "Reason and recovery wish" only: the request is stored with **1
  instalment**, the full amount in the first month after the payout. The approver at any stage (usually the line
  manager) picks **Recover in 1 to N monthly instalments** when approving (`POST /api/approvals/{id}/approve {note,
  instalments}`, N = `company_setting.adv_max_instalments`, default 12); the history reads "Instalments set by <name>
  (<stage>) when approving: …" and the audit log "Advance instalments set". Payroll's **Change recovery** at its step
  (`advance-terms`) stays, capped by the same N. Both settings sit with the advance limits (Payroll › Pay schedule and
  components › Salary advances; `PUT /api/advances/settings {minMonths?, maxInstalments?}`). Advances Payroll or HR
  raise from Employee dues keep their own instalments and are not bound by the service rule.
- **Sample data:** the sample organisation's staff now have a UAN (10010000 and the employee number), so the UAN hold
  holds nobody there.
- **Data on restart:** no migration. New optional fields: `employee_pay.pf_exempt`, `pf_exempt_reason`,
  `pf_exempt_at`, `pf_exempt_by`, `pf_exempt_manual`, `var_start`; `company_setting.pf_exempt_gross`, `uan_hold`,
  `adv_min_months`, `adv_max_instalments` (null = the defaults); advance payloads may carry `instalmentsBy`. Registry:
  `internal/store/schema_pfuan.go`.
- **Parity:** two new GET routes (`/api/payroll/pf-settings`, `/api/employees/{id}/pf-exempt`, in
  `tests/parity/routes.mjs`); `/api/advances/settings` and `/api/me/advance` rules gain `minMonths` and
  `maxInstalments` (`/api/me/advance` also `instalments`); the employee's `salary` gains `varPayout` and `pay.pfExempt`,
  `pay.varStart`; the checklist's bank closure gains `uan` and `uanN`; pay rows may carry `r.pfExempt`; the full
  register gains the Incentive column. The Postgres build had none of these.
- **Tests:** `internal/api/pfuan_test.go` (`TestPfExemptJoiner`, `TestUanHoldAndRelease`), `pfuan_unit_test.go`
  (`TestPfExemptRuleUnit`, `TestVariableOwnCycleOwnersExample`), `bulk_incentive_test.go` (`TestBulkIncentives`),
  `advances_test.go` (`TestAdvanceEligibilityAndInstalments`), `internal/calc/calc_test.go`
  (`TestVariablePayoutOwnCycleFromJoining`, `TestPfExemptCalc`). Changed: `TestSelfAdvance` (an employee's
  instalments are no longer taken; the history now reads "1 instalment → 6"), `TestPayfixVariableFromEffectiveMonth`
  (variable pay that starts in the open month starts its cycle there, so no payout that month),
  `TestUI3PayrollStatutory` (EMP-1003 has its UAN removed to stay a "new UAN" joiner, now that the sample staff have
  one); browser `tests/e2e/advances.mjs` (no "Recover in" on the employee's form; the first approver sets 3
  instalments). Manual steps: Part M of `docs/manual_test.md`.

### Access by person and area, policy conflicts, leave by employment type (workforce plan modules 2 and 5, 17 Nov 2026)

- **Personal rights** (Access control › Personal rights, and Roles and access on the employee file): give one person a
  single right on top of their roles (View or Edit), or block one (leaves No access or View), with an optional last
  day and a reason. Effective rights = the role view's rights + grants − blocks; a block wins. Grants work within the
  data access of the role view in use. HR Admin always has every right and cannot be blocked; only an HR Admin gives
  or removes HR Admin, and the last active HR Admin cannot be switched off. Someone else with Role mapping edit gives
  a right only up to their own level. Stored in `user_right` (migration `011_access_policy`); every change audited.
- **Data access by state, city or cluster** on a role mapping (`access` = states | cities | clusters, `area_ids`).
  The locations inside are worked out on every request, so a location added to a covered cluster is covered at once.
  Mappings by locations are unchanged.
- **Who can see what** (Access control › Check access, below the person): each right, the level the person ends up
  with and where it comes from (role, personal grant, block, HR Admin), with Download.
- **Policy conflict check** (Work policies › a policy › Mappings): before a mapping is saved the app previews who it
  would cover that already gets another policy through another mapping ("Operations – 24x7 rotational is already
  applied here (through the Front Office department) to 3 people"). HR chooses Replace, Keep, Pick people or Cancel;
  the choice is saved as employee-level exceptions (marked Exception, with a note), so the result never depends on the
  order of the levels. It applies at once (mappings carry no dates). The API refuses a conflicting save without a
  choice (409). Employee-level mappings are never conflicts; the same mapping moving from another policy is shown as
  a move. Coverage has an **Overlaps** report (people matched by mappings of two or more policies, with Download), and
  the employee file shows the **Work policy** the person gets and why.
- **Company policies** can also be for cities, clusters or named employees (named people are added on top of the
  groups; being in any chosen city, cluster or location is enough).
- **Leave by employment type** (Compensation › Salary structure › Rules by employment type › Leave): All leave types,
  Only chosen types, or No leave. Enforced on leave requests, balances (accrual), opening balances (Bulk update and
  import) and PL encashment at exit; the leave plan still decides days and rules.
- **Routes:** `GET/POST /api/user-rights`, `DELETE /api/user-rights/{id}`, `GET /api/access/person/{id}/effective`
  (`.xlsx`), `POST /api/policies/{id}/maps/preview`, `PUT /api/policies/{id}/maps` takes `resolution`
  {mode: replace | keep | pick, move: [ids]}, `GET /api/policies/overlaps` (`.xlsx`), `GET /api/employees/{id}/policy`.
- **Tests:** `internal/api/access_policy_test.go` (`TestPersonalRights`, `TestScopeByArea`, `TestPolicyConflictCheck`,
  `TestCompanyPolicyAudienceAreas`, `TestLeaveEligibilityByType`). Changed: `TestPolicyMappingOrder` switches its
  overlapping mappings on in the database (the API now refuses them without a choice). Manual steps: Part P of
  `docs/manual_test.md`.

### Hiring lifecycle: candidate › Offer ID › Joining ID › Employee ID (workforce plan module 1, 17 Nov 2026)

- **Offer versions, no Offer ID until acceptance.** An offer is a version under the candidate (SI-0042 offer v1, v2…;
  `offer_version`). Selected at the interview no longer issues the OI- number: HR records the candidate's answer on
  the candidate (no candidate link): **Accepted** issues the Offer ID (OI-, same number) and freezes that version;
  **Question about the breakup** (what was asked) lets HR revise, which raises the next version for approval and sends
  it again; **Declined** (reason) closes the candidate. Release sets the answer date (Form setup › Hiring, 7 days by
  default; HR can move it with a reason); the hiring sweep reminds HR before it and closes the case as **Offer lapsed**
  after it. Candidates already selected keep the OI- they were given (migration `010_hiring_lifecycle` fills
  `offer_no`; it changes no employee). The offer's internal key stays OI-nnnn, so approvers still see it on the offer
  approval request.
- **Before and at joining:** withdrew before the joining date, joining date moved (new date, reason), came and declined
  on the day, did not come (Waiting), or joined on the actual date.
- **Waiting window** (Form setup › Hiring: 7 days by default, HR confirms the close or it closes automatically): a
  no-show on the joining day and someone who stops coming in the first days both go to the pipeline's Waiting column
  with a day counter ("day 3 of 7"), a call log, extensions for one person with a reason and a daily bell to the HR who
  raised the candidate. Back within the window: a no-show is marked joined on the actual date (same offer, documents
  and link); someone who stopped coming resumes and the missed days stay absent. Window over: **No-show**, or **Early
  exit**. Saying they quit closes as Early exit at once. Early exit = the record leaves with the last working day (paid
  for the days worked by normal payroll), exit reason Early exit, a rehire flag, no separation or F&F case.
- **Joining ID record.** Mark joined makes the employee record at once under JI-0042: Active, stage Joining, punches
  (`ingestPunches`, attendance code), attendance and payroll from day 1. The joining details go onto the record when HR
  sends them to Payroll. **Employee ID** once every step is approved (mandatory details and documents, Payroll
  verification, and the background check when it blocks): the next code of the series, and the record moves to it in
  one transaction (`store.RekeyEmployee`: every field the schema registry knows as an employee reference, plus an
  `employee_id` field in collections it does not list; the audit log and JSON snapshots such as payslips keep the ID
  of the time). JI-0042 stays in `employee.alias_ids`: `/api/employees/JI-0042…`, `?employeeId=JI-0042`, the people
  search and `/api/candidates/{Employee ID}` still find the person. Candidates who joined before this keep the old rule
  (Employee ID when the documents are complete).
- **Background check** on the candidate: Pending, Clear or Discrepancy with the uploaded report (document
  `bgvReport`). Setting "holds the Employee ID", on by default.
- **Employment types without an Employee ID** (Employees › Employee codes › Employee ID: None): at joining the person
  takes the next number of their type's own series (e.g. CON-00001) for good; paid and tracked like everyone else.
- **Status timeline** (`lifecycle_event`): every candidate and employee change with who, when, from → to and why, on
  the candidate and on Employee file › Status timeline (with status changes from the employee change history).
  `employee.stage` (Joining, Probation, Confirmed, On notice, Suspended, Long leave, Absconding) is separate from the
  status (Active filters are unchanged); HR with `emp.status` at Edit sets it with a reason.
- **Hiring drop-off report** (Reports › Workforce): offer declined, lapsed, withdrew, no-show, declined on the day and
  early exit, by month, property, role or recruiter.
- **Routes:** `POST /api/candidates/{id}/event` (action doj | withdrew | declined-day | no-show | stopped | quit |
  extend | call | back | close), `POST /api/candidates/{id}/bgv`, `POST /api/offers/{id}/query` and `/expiry` (release
  takes `expires`), `POST /api/employees/{id}/stage`, `GET /api/employees/{id}/timeline`; `GET/PUT
  /api/onboarding/settings` take `hiring`; `GET/PUT /api/employee-codes` take `noEmpId`; report `hire_dropoff`. Job
  `hire_sweep` every 30 minutes.
- **Tests:** `internal/api/hiring_test.go`; the onboarding stage tests were updated to the new flow. Manual steps:
  Part Q of `docs/manual_test.md`.

### Punch machines: the cluster is one pool (workforce plan module 7, 17 Nov 2026)

- **Model.** Each machine (T304F Mini) holds about 1,000 people; the machines of one cluster are one shared store, so
  a person can punch at any machine of their cluster. A move between properties of a cluster needs nothing; a move to
  another cluster needs enrolment there and removal from the old one. Code in `api/machines.go`,
  `internal/realtime/machines.go`, `store/schema_machines.go`; page Attendance › Punch machine setup › **Punch
  machines by cluster** (`MachinePool.tsx`). Rights `att.setup` (View to see, Edit to change).
- **Machines from the portal.** "Import machines from the portal" opens the Realtime portal's `MachineList.aspx` (read
  only: a GET, no button pressed; added to the client's whitelist) and creates or updates an `attendance_device` per
  serial number (vendor Realtime, model, machine no, In/Out mode, portal branch). A machine the list no longer shows
  is marked "no longer in the portal's list", never removed. HR maps each machine to a property (its cluster follows);
  where the portal branch or machine name matches one active property's name ("SaltStayz" and the like ignored) it is
  suggested, and "Use the suggested properties" maps them all. Capacity per machine, or the default (1,000, changeable).
  The portal's machine list has **no online/offline column** (17 Nov 2026): the board says "Not shown by the portal";
  a status column, if the portal adds one, is read and shown. No alerts.
- **Board.** Per cluster: machines, capacity (its active mapped machines), people (active employees, Joining-ID
  records included, whose property is in it), free, % used, status OK / Nearly full (from 90%) / Full / No machines,
  properties without a machine, open to-dos. A cluster with no machine mapped is never checked.
- **Full cluster.** Mark joined (`hireJoin`) and every job change that moves someone to a property of another
  cluster (`ApplyJobChange`: single edit, bulk update, approved transfer) are refused with 409 (`details.kind
  "cluster_full"`) when the cluster has no free place; the web app asks "Go ahead anyway?" and repeats the request
  with `?machineOverride=1` (or header `X-Machine-Override: 1`), which is audited. `GET /api/attendance/machines/check
  ?locationId=&employeeId=` gives the same check (level ok/warn/full/none) for pages that want to warn first; Go
  callers use `machineCheck` / `machineGate`.
- **Where each record is stored** (`mpPlan`, worked out on every read): own property's machine(s) first; the rest on
  the next machine of the cluster with space, going round from just after their property's machines. Earliest joiner
  first, so a joiner never moves anyone. "Where records go" shows it per cluster.
- **To-do list** = the difference between where each active employee works and the HRMS's record of who is enrolled
  on which cluster (`machine_enrol`, one row per person): joiner "Enrol in cluster X (machine Y)", exit "Remove from
  cluster X", move to another cluster "Enrol in B, remove from A"; a move inside a cluster gives nothing. Tick done by
  hand, or automatically when the portal's employee list is fetched (Match portal codes): the Attendance code in a
  branch of cluster X (enrol) and in none of A (remove); the rest get a note. Excel download. Migration
  **`013_machine_pool`** creates `machine_enrol` and records every active employee as enrolled on their cluster (they
  punch today); after an import from PostgreSQL, "Record everyone as enrolled" does the same once.
- Routes: `GET /api/attendance/machines`, `…/todo` (`?kind=`), `…/todo.xlsx`, `…/plan?clusterId=`, `…/check`;
  `POST …/import`, `…/{id}/map` (`locationId`, `capacity` or `defaultCapacity: true`), `…/use-hints`, `…/settings`
  (`defaultCapacity`), `…/baseline`, `…/todo/done` (`employeeIds`). Settings and the last import and portal check in
  `company_setting.machine_pool`. Not built: flagging a punch from another cluster's machine (the portal's punch report
  has no machine column). Tests `TestMachinePlan`, `TestMachinePool`, `internal/realtime` `TestParseMachines`,
  `TestClientMachineList`.

### Sending employees to the Realtime portal (automatic since 19 Nov 2026)

- **What.** The HRMS writes to the Realtime India portal by itself: **Mark joined** creates the employee (code = the
  ID's digits, 6 wide, which becomes the Attendance code); a change of the details the portal gets (name, department,
  designation, property/branch, shift, phone, email…) of someone the HRMS put on the portal updates them; an exit
  (inactive, or the last working day reached) marks them left. There is no "Send to portal" button and no preview
  any more. Code: `api/portal_auto.go` (jobs, sweep, Retry, backlog), `api/portal_write.go` (details, mapping,
  status, waiting list), `internal/realtime/empwrite.go` (`Post`; form fill), `store/schema_joinauto.go` (migration
  `022_join_auto`), `PortalCard.tsx`, `PortalSend.tsx`. Right `att.portal` (View: statuses and list; Edit: map,
  Retry, Send waiting).
- **How.** Each write is a Mongo job (`portal_write`), inserted in Mark joined's transaction or by the every-minute
  `portal_sweep` that notices changes and exits; never inside a request. The job opens the portal's form afresh,
  stops on any problem before the post, and posts **once**: a job is never retried (one attempt), and a failed write
  stays failed until HR presses **Retry** (employee card or the waiting list). A refusal or error, a missing mapping
  (held, "Map the department … first") and an unclear reply ("Check on the portal", settled by HR) go to Attendance ›
  Punch machine setup › **Realtime portal: automatic sending** with the portal's message, and HR (HR Admin and
  `att.portal` Edit) gets a bell. Every write is audited with the fields sent and the reply, and logged in
  `portal_send` (by "HRMS (automatic)", or the HR name on Retry). State in `employee.portal_sync` (`want`, `phase`
  waiting/sending/failed/blocked, `phase_msg`).
- **Which people.** Created: joiners marked joined, and active people without an Attendance code who joined on or
  after `company_setting.portal_write.auto_since` (the day of migration 022). Marked left: people the HRMS sent, and
  people with a code whose last working day is on or after `auto_since`. Updated: only people the HRMS sent (their
  last details are known). A person "Match portal codes" proposes a portal record for is held, not created twice;
  "Retry: they are someone else" creates them.
- **Details sent.** Code (the digits of the ID, 6 wide: JI-000006 → 000006, 25 Nov) and card number (the code padded
  to 8 digits), name, father's name, birth and joining dates (the shift start date = the joining date on a create),
  gender, phone, email (left out, and said so, when the portal's own check would refuse the address), and the
  portal's department, designation, branch and shift as mapped once under "Mapping and code rule" (a property
  defaults to the portal branch of its mapped machine), plus the portal's **Office Time Policy** and **First Weekly
  Off** as mapped for the employee's work policy (kinds `policy` and `wo`, 25 Nov). The portal's company is left as
  its form has it (one choice, Saltstayz). After a create the machine enrol to-do is ticked when the branch is in the
  target cluster; a mark-left ticks the remove to-do. The portal's device step ("Upload Name in Device") is never
  pressed.
- **Punch machine on a create (owner, 25 Nov 2026).** A create also picks the machine in the form's **Select Devices**
  list (`Device_List`, detail `realtime.FieldDevice`): the machine the machine pool plans for the person (Attendance ›
  Punch machine setup › machine pool: their own property's machine first, then the next machine of the cluster with
  space, each machine holding the capacity the admin set), matched by its serial number (the option's value, or the
  start of its text "RSS20230761581 / Gulmohar Marg"). No machine (property without a cluster, no machine mapped to
  the cluster, the cluster full, a machine without a serial, or a serial the portal's list does not have) does **not**
  hold the create: it goes with the form's own choice and a note "No punch machine chosen: …; HR adds them to a
  machine on the portal". The machine chosen and the notes are kept in `portal_send` (`device`, `notes`, no
  migration), in the audit, and shown under "Writes" on the employee's Realtime portal card. An update or a mark left
  never sets the device. The form's device buttons (Connect, Upload Name, Enable/Disable/Delete User) are never
  pressed (`neverPress`). Code: `ptDevice` (`api/portal_auto.go`), `pickDevice` (`realtime/empwrite.go`).
- **The portal's form (read 25 Nov 2026, GETs only).** The portal has no API for employees: `Third_PartyApi.aspx`
  only pushes punches out to another system (greytHR, ZingHr or a URL). The employee form is `Employee.aspx`:
  the list's **Add** gives `Employee.aspx?RowId=0` (confirmed by the owner, 25 Nov; the blank form; without a RowId the page fails), each row of
  `EmployeeList.aspx` links to `Employee.aspx?RowId=<row>`. A write reads the list (GET) first: a create stops when
  the code or card is already there ("The portal already has employee code …"), an update or mark-left finds the
  employee's RowId by code. Controls (`ctl00$MainContent$…`): `Txtpaycode` code, `Txtcardno` card (8 digits, 9 at
  most), `Txtempname`, `txtfartherhusname`, `txtDateofBirth`, `TxtPhoneNo`, `TxtEmailAddress`, `CboGender`,
  `cbocompnayname`, `cbobranchname`, `cbodeptname`, `cbodesignation`, `cboofficetimepolicy`, `TxtDOJ`,
  `txtshiftstartdate`, `cboshiftname` (GEN, NST, SFT), `cboFWOff` (Sunday … Saturday, None) and
  `txtreginnationdate` (**mark left = the Resignation Date**; the form has no status, and "Auto Approved Gps Punch"
  is not one). Required by the page: name, code, card, company, department (its blank first choice is not one),
  designation, office time policy, shift. Everything else (shift type, second weekly off, branch-access ticks,
  mobile attendance mode; on an update or mark left also the device list) is posted as the form has it. The only button pressed is **Save**
  (`cmdsave`); Connect, upload Name, Enable/Disable/Delete User, Cancel and the OTP buttons never are. After the
  post the HRMS looks again (GET): a create must be on the list, an update or mark-left must show the values sent
  on the employee's form; otherwise the write is "Check on the portal". The page's own script alerts and labels are
  not taken for the reply. Fake of this form: `internal/realtime/rtfake`.
- **Switched on (owner, 25 Nov 2026).** `realtime.Spec` holds the live form and sending is **on** (`Spec.Send =
  true`); the owner confirmed the employee list's **Add** button is how employees are added and that it opens
  `Employee.aspx?RowId=0` (confirmed by the owner, 25 Nov). Writes due
  from now go out by themselves; whatever waited while sending was off stays Waiting until HR presses **Send waiting**
  once. Still to watch on the first live writes: what the portal says after Save (the HRMS's look on the list/form
  covers it), that the Resignation Date alone marks someone left, and the shift list's own post-back when the shift
  changes. To switch it off again: `Send: false`, rebuild (every write due then waits, "Sending is switched off";
  reading the lists and mapping still work). Tests `TestPortalAutoCreate` (create with and without a machine, cluster
  full, update, mark left), `internal/realtime` `TestEmployeeWrites`, `TestSendOff` (the off path), `TestDeviceOnCreate`,
  `TestListRows`.

### Company accommodation from the offer (19 Nov 2026)

- The offer form and the release ask **Company accommodation: Yes / No**; Yes needs a cluster that has accommodation
  units (`offer.data.accom`, `accomCluster`; `GET /api/hst/clusters` lists them with free beds). The tag shows on the
  candidate and the offer (`{{accommodation}}` in the letter template); it has no pay effect.
- At **Mark joined** the HRMS allocates a bed by itself in the same transaction: the cluster's active units matching
  the joiner's gender, in the suggestion order (the unit serving their property first, then the most free beds),
  check-in on the actual joining date, note and timeline "Allocated automatically from the offer", audited.
- No free bed: the joiner goes on Accommodation › **Waiting for a bed** (`employee.hst_wait`, `GET /api/hst/waiting`)
  and HR (`hst.allot`) gets a bell; the maintenance sweep (every 5 minutes) gives them the next bed freed in that
  cluster (check-in that day). A check-in by hand takes them off the list; "No longer needed" drops them (reason,
  audited). Code `api/hst_auto.go`; test `TestJoinAccommodation`.

### Staff accommodation: flats, leases, beds and the true cost (workforce plan module 4, 17 Nov 2026)

- **Page** Accommodation (left menu; rights `hst.setup` units, leases and costs, `hst.allot` check-in and check-out;
  View 1 / Edit 2; HR Admin has both, other roles none until given in Access control). Tabs: Occupancy board, Units
  and leases, Check-in and check-out, Month cost split, Settings. The employee file has an **Accommodation** tab
  (stays, check-outs to do, "Assign accommodation"). Code `api/hst.go`, pure split `internal/hst`, page
  `Accommodation.tsx`, schema `store/schema_hst.go`.
- **Units** (flat, house, PG): name, address, cluster, the properties it serves in order of nearness (the first is
  its home property), gender (Male, Female, Any), beds, landlord and mobile, status, other fixed monthly items.
  **Leases** with history (never deleted, never overlapping): start, end or ongoing, notice days, rent, deposit,
  maintenance, yearly increase % and the date of the first increase (it repeats yearly; rent is raised day by day
  from that date). Documents per lease through `/api/files` with `ownerKind=hst_lease` (agreement, landlord ID,
  receipt, other). **Month bills** per unit: electricity, water, Wi-Fi, housekeeping, other.
- **Allocation.** "Assign accommodation" lists the units of the person's cluster with a free bed from the date, the
  ones serving their property first, matching gender (an Any unit fits everyone; someone without a recorded gender
  fits every unit). Check-in picks the first free bed or a chosen one. A stay covers check-in ≤ day < check-out, so
  the check-out day is free for the next person. A bed is never double-booked and a person never has two stays at
  once (checked under the unit's and the person's locks); beds cannot be reduced below a bed in use and a unit with
  people cannot be made inactive. Stays can be corrected (same checks). People still checked in after they left, with
  a last working day, or after moving to a property outside the unit's cluster are listed under "To check out".
- **Cost split (decided 17 Nov).** A unit's month cost = rent and maintenance of the lease in force each day + fixed
  items while leased + that month's bills. **Equal per person** (the default): the month cost ÷ the number of people
  who stayed in the unit at any time in the month (12k with one person = 12k; two = 6k each; four = 3k each, however
  many days each stayed). Only a month with nobody in the unit at all is **empty-unit cost** for its home property.
  Settings › **Split by days stayed** (off by default) shares each day's cost (month cost ÷ days in the month) among
  the people staying that day instead, and a day with nobody is empty-unit cost. Shares are to the paisa and cost =
  allocated + empty exactly (equal mode gives a rounding paisa to the first person). `api.HstMonthCost(ctx, q, ym)`
  follows the chosen method and returns per-employee shares and per-property empty cost for the cost console.
- **Not payroll (decided 17 Nov).** Company accommodation is only the company's own cost calculation. Nothing of it
  appears on the payslip, salary register, payroll sheet, TDS or any payroll total, and an accommodation change never
  marks a payroll run for recalculation (`TestHstPayrollUnaffected`: a month with and without a stay gives identical
  rows). The employee file's Accommodation tab shows the stays only. The manual employee accommodation deduction
  (`employee_pay.accommodation`, the minimum-wage top-up, "Accommodation Deduction"/"Accommodation" bulk columns) is
  separate and unchanged: it still shows on the payslip and salary sheet. The first build's statutory switches
  (`company_setting.hst.stat`) are dropped by an idempotent heal on start (`healHst`).
- **Reminders** (bell, the maintenance job every 5 minutes, once each): a lease ending within 60 days (Settings), the
  last week to give the landlord notice, a rent increase within 30 days (to HR Admins and roles with `hst.setup`
  Edit), and a stay to check out after an exit or a transfer (to HR Admins and roles with `hst.allot` Edit).
- **Occupancy board** per city and cluster: units, beds, filled, free (male, female, any), month cost, allocated,
  empty-unit cost, leases ending in 60 days; Excel download.
- Routes: `GET /api/hst/setup`, `PUT /api/hst/settings`, `GET|POST /api/hst/units`, `GET|PUT /api/hst/units/{id}`,
  `POST /api/hst/units/{id}/leases`, `PUT /api/hst/leases/{id}`, `PUT /api/hst/units/{id}/costs/{ym}`,
  `GET /api/hst/suggest?employeeId=&date=`, `GET|POST /api/hst/stays`, `PUT /api/hst/stays/{id}`,
  `POST /api/hst/stays/{id}/checkout`, `GET /api/hst/attention`, `GET /api/hst/board?ym=`, `GET /api/hst/board.xlsx`,
  `GET /api/hst/split?ym=`. Migration **`012_accommodation`** creates `hst_unit`, `hst_lease`, `hst_cost`, `hst_stay`
  (additive). Tests `internal/hst` (equal and by-days examples, empty unit), `api` `TestHstAllocationAndSplit`,
  `TestHstPayrollUnaffected`. Manual steps: Part S of `docs/manual_test.md`.

### Pay structure designer: components, fixed and variable (workforce plan module 3, 17 Nov 2026)

- **Existing pay does not change.** Every salary structure made before this change is converted (migration
  **`014_pay_components`**) into an equivalent component template stored in `salary_structure.rates.comps`: Basic + DA
  as % of gross, HRA as % of Basic, Special allowance as the remainder (PF wage when the version's PF wages are basic +
  special allowance) and Variable pay as "legacy accrued % of gross" (`legacy_var`), which keeps today's automatic
  accrual and payout on the person's own cycle (`varpay.go`). The engine evaluates that template with the same
  arithmetic in the same order as the old calculation, so the result is identical to the paisa; a structure without
  `comps` is converted on the fly. Saving a converted version's rates in Salary structure rewrites its template.
  Proof: `hrms pay-compare -month YYYY-MM[,YYYY-MM]` calculates everyone paid in a month both ways from the same data
  (`calc.Input.Classic` is the old calculation) and lists every difference; exit status 1 when there is one. Tests
  `calc.TestLegacyComponentsSameAsClassic` (20,000 random inputs, bit for bit) and `api.TestPayCompareLegacyUnchanged`
  (the sample organisation over five months with a second percentage version mapped mid-month, personal variable %,
  cycles, meal deductions and a mid-month gross revision, on the fly and stored).
- **Component library** (Compensation › Pay components, `comp.structure` View/Edit; collection `pay_component`):
  name, code, Fixed or Variable, earning or deduction, role (Basic, DA, HRA or none), method (fixed ₹ a month; % of
  CTC; % of fixed gross; % of Basic; % of another component; remainder) with an optional minimum and maximum, and the
  flags taxable, PF wage, ESI wage, bonus wage, shown on the payslip. Variable components have a payout cycle:
  monthly, quarterly, half-yearly or yearly, counted from the person's joining month (since 18 Nov; see "Pay fixes of
  18 Nov" below). Variable pay is never PF, ESI or bonus wage (as before). A deduction is not prorated.
- **Structures from components** are salary structure versions like any other: named, made default, switched off,
  and mapped to people from a date (Payroll › Structure mapping; a date inside a month blends the month by days).
  The PF, ESI, bonus, medical, gratuity and days settings are copied from a version; the components are a copy of the
  library entries with the structure's own method and value. Checks: exactly one fixed earning is the balancing
  figure, one is marked Basic, references exist, no loop. Like rates, a change applies to every month not yet
  approved; to change pay from a date, make a new structure and map people to it.
- **Statutory reading of components.** PF wage = the PF-flagged fixed earnings (capped as the version says); PF admin,
  EDLI, gratuity and PL encashment on Basic + DA; ESI contributions on the earned gross less the components that are
  not ESI wage (eligibility on the full gross, as before); bonus on the bonus-flagged earnings; TDS projection less
  the components that are not taxable, plus variable amounts entered in the year.
- **Entering pay** by CTC (here: fixed + variable a month) or by the fixed monthly gross, the variable part as ₹ or %
  of the total (₹35,000 = ₹20,000 fixed + ₹15,000 variable): Compensation › Pay components preview
  (`POST /api/comp/pay-preview`), the salary calculator, the offer ("Enter pay by CTC" under the Compensation card; a structure
  made from components also takes "Variable target, ₹ a month", kept from the joining month in `var_target`), and
  Bulk update › Salary breakup's two optional columns **Total Pay (Fixed + Variable)** and **Variable Target**. On a
  percentage structure the variable part becomes the person's variable % of gross.
- **Variable pay is a manual amount (decided 17 Nov)** for designed structures: each person has a monthly target
  (`var_target`, dated; total and optionally per component, else each component's method shares it); the month's
  accrual counts in the CTC; when a payout is due HR enters the amount (Payroll › **Variable pay**,
  `PUT /api/pay/variable`, or the optional payout columns of Bulk update › **Salary breakup**, `bulk_varpay.go`;
  collection `var_payout`). Since 17 Nov the separate Variable payouts template is merged into Salary breakup:
  **Payout Month**, **Payout Component** (blank when the structure has one variable component), **Variable Payout (₹)**
  (blank: no payout) and **Payout Note**. A row may change the salary, give a payout, or both; the payout is checked
  (due month, open month or later, ≥ 0) before anything in the row is written. Salary columns keep their right (Bulk
  update and Salary at Edit), payouts theirs (Payroll run or claims at Edit, or HR Admin); a payouts-only file needs
  no salary columns, and the current-data download fills the payout columns for each component due in the open
  month (a second due component on a row of its own). Not
  entered: not paid, the row lists it (`varMissing`) and the payroll checks show "Variable pay due but not entered".
  The full and final shows each variable component of the current cycle; HR enters an amount there
  (`PUT /api/fnf/{id}/variable`), paid as an earning of the settlement.
- **Compliance.** A warning, never a block, when Basic + DA is under 50% of total pay (fixed + variable, the labour
  codes' wage rule): on the structure (lowest share at ₹15,000/30,000/60,000 gross) and under every breakup.
- **Where components show.** The breakup on the offer, salary calculator and previews; the offer letter's new merge
  fields `{{basic}}`, `{{hra}}`, `{{total}}`, `{{ctcMonth}}` and `{{breakup}}` (one line per component); the payslip
  lists a designed structure's components by name (rounding to the balancing component; components not on the
  payslip as "Other allowances"); the full salary register adds a column per component (fixed ones as "Component: …",
  a breakdown of Basic/HRA/Special allowance; variable as "Variable: …"; deductions after Accommodation) only when a
  row has one; the payroll sheet and register sheet end with "Component deductions" and "Component details" when a row has
  components (a month of percentage structures gives exactly the sheet it gave before). The manual meal and
  accommodation deductions and staff accommodation are untouched.
- Routes: `GET|POST /api/comp/components`, `PUT /api/comp/components/{id}`, `GET|PUT /api/comp/structures/{id}/components`,
  `POST /api/comp/structures/designed`, `POST /api/comp/pay-preview`, `GET|PUT /api/pay/variable`,
  `PUT /api/pay/variable/target`, `PUT /api/fnf/{id}/variable`. Code `calc/components.go`, `api/paycomp.go`,
  `api/paycompare.go`, `api/bulk_varpay.go`, `store/schema_paycomp.go`; pages `Compensation.tsx` (Pay components),
  `VariablePay.tsx`, `components/PayEntry.tsx`. Tests `calc/components_test.go`, `api/paycomp_test.go`,
  `api/paycomp_unit_test.go`. Manual steps: Part T of `docs/manual_test.md`.
- **Variable pay as a target on every structure, Version 1 included (decided 17 Nov 2026; no Version 2).** Migration
  **`017_variable_target`** converts each percentage structure in place (same id, so Structure mapping is untouched):
  its components become the same fixed components plus one manual "Variable pay" (the person's whole target, on the
  version's standard payout cycle), `rates.designed`/`rates.converted` are set and the standard variable % is retired.
  Each person's variable % (`employee_pay.var_pct`, else the version's standard %) becomes a `var_target` row in mode
  **%** with the same value from the month it applied, `var_pct`/`var_cycle` are cleared and Employee history records
  the move. Fixed pay is unchanged to the paisa; the only change is that variable is no longer paid automatically.
  Saving a converted version's rates still moves its fixed components (Basic %, HRA %, PF wages); its payout cycle is
  the "Standard payout". The automatic `legacy_var` method stays in the engine, unused. Every start (`hrms serve`) also converts a
  structure still on percentage rates (a new installation's seeded Version 1), and "New version" makes target
  structures, so % or ₹ can always be chosen.
- **% or ₹, chosen each time.** Wherever variable is entered (offer, Pay options in the employee file, salary
  calculator, Try an offer, Payroll › Variable pay, Bulk update › Salary breakup) the admin picks "% of gross" or
  "₹ a month" (never locked to one) and the value; a % shows its ₹ amount on the gross. A % target is worked out on the full fixed monthly
  gross each month (it follows gross revisions); ₹ is a fixed amount a month. `var_target.mode` (`pct`|`amount`) and
  `pct`; offers keep `varMode` + `varPct`/`varAmt` and carry the target to the employee at joining. The bulk template
  adds **Variable Target In** (% or ₹; blank keeps the current way) beside Variable Target; Variable Pay % and
  Variable Payout Cycle are refused for a structure on targets.
- **Payout stays manual.** Payroll › Variable pay suggests the target a month × the months in the cycle ("Use ₹…"),
  editable; not entered = not paid plus the payroll check.
- **Before/after proof for a data change:** `hrms pay-compare -month M1,M2 -dump before.json` before, then
  `hrms pay-compare -month M1,M2 -against before.json` after; lists every field that differs (component lines are
  display only and left out). Test `calc.TestVarTargetPctAndAmount`. Code `store/schema_vartarget.go`.

### Cost and budget console (workforce plan module 6, 17 Nov 2026)

- **Page** Cost and budget (left menu; rights `cost.view` View 1 to see it, `cost.budget` Edit 2 to change budgets; HR
  Admin has both, other roles none until given in Access control). Limited to the viewer's properties (a role mapped
  to properties, clusters, cities or states sees those; team or self views get 403). Code `api/cost.go`, page
  `Cost.tsx`, schema `store/schema_cost.go`, migration `015_cost_console` (the `cost_snapshot` collection; cluster
  budget columns; additive only).
- **Rows** company › state › city › cluster › property (expandable), with a drill-down to each person (needs salary
  figures in full) and a six-month trend for any row. **Columns:** people, salary paid, company contributions,
  accommodation, total cost, cost per head, budget, difference, % used (red and "Over" when over budget).
  - *Salary paid* = earnings (fixed, variable, incentives, arrears, claims) less the bonus and less the **minimum-wage
    top-ups**: the manual meal and accommodation deductions (`employee_pay.meal` / `.accommodation`) raise the gross
    and are deducted back, so they are no company cost and count against no budget (owner, 17 Nov). The amount left
    out is shown under the table.
  - *Company contributions* = employer PF, EPFO admin and EDLI, employer ESI, LWF, gratuity, bonus, medical insurance,
    as paid (no adjustment).
  - *Accommodation* = each person's share plus empty-unit cost on the unit's home property (`api.HstMonthCost`).
    Accommodation not tied to any property shows as "Not tied to a property" (whole-company view only).
  - There is no expense ledger: the company has no other costs today (decided 17 Nov).
- **Actual or projected.** An approved or published month reads its payroll rows. The open payroll month (and up to
  12 months after) is **Projected** from the current salary structures: everyone paid in the month, full month, with
  the pay lines already entered. An earlier month without an approved run shows "No approved payroll".
- **Frozen.** Publishing a month writes its snapshot (`cost_snapshot`: people, empty-unit cost, places and budgets as
  they were) after the publish commits; the page then shows "Frozen on <date>" and never changes for that month. A
  month published before this build (or a failed freeze) is frozen by the 5-minute maintenance sweep
  (`CostFreezeSweep`).
- **Budgets.** Property: the existing location fields (`payroll_budget` = monthly cost budget, `headcount_budget` =
  seats), also on Compensation › Budgets. Cluster: its own `cost_budget` / `headcount_budget` when set, else the total
  of its properties. City, state and company add up their clusters. Editable on the page (property and cluster rows,
  not on a frozen month), audited "Budget changed (cost console)". Bulk update › Budgets takes a **Cluster** level
  (Monthly Cost Budget (CTC) = the cluster's cost budget; 0 = back to the total of its locations).
- **Headcount plan** tab: per cluster and property, seats against people in post (active, Joining IDs included),
  offers in flight (accepted and not joined, and pre-boarding records) and offers in approval; free seats, red when
  over. The offer budget check (`budgetOver`) counts the same numbers (`seatsUsed`) and also checks a cluster's own
  seat budget; its location cost check uses the total monthly cost (see "Pay fixes of 18 Nov" below).
- Routes: `GET /api/cost?ym=`, `GET /api/cost/people?ym=&kind=&id=`, `GET /api/cost/trend?ym=&kind=&id=`,
  `GET /api/cost/headcount`, `GET /api/cost/export.xlsx?ym=` (Excel of the current view),
  `PUT /api/cost/budget/{location|cluster}/{id}`. Test `api/cost_test.go`. Manual steps: Part U of
  `docs/manual_test.md`.

### Pay fixes of 18 Nov 2026: variable cycle from joining, cost budgets, retainer fee

- **Variable pay cycle from the joining month.** A designed structure's variable component is due when a cycle counted
  from the person's joining month is complete (`calc.VarDue`): quarterly, joined May 2026 → July, October, January,
  April; half-yearly → October, April; yearly → April the next year; monthly every month. The anchor is
  `employee_pay.var_start` when variable pay started after joining, else the joining date of the current service (a
  rejoiner's record carries the new date). Used by Payroll › Variable pay (each component shows "From …, next …"), the
  payroll check "Variable pay due but not entered", `PUT /api/pay/variable`, the Salary breakup payout columns and the
  employee's pay section ("Own cycle from …: next payout …"). Before, cycles ended with the financial year's quarters.
- **No target, no payout.** A payout above 0 for someone with no variable target (none or 0 in every month of the
  cycle) is refused with "… has no variable target …" (screen and bulk); the list shows "No target: set one first" and
  does not count it as due. Entering 0 or clearing is allowed. The full and final keeps taking an amount for the
  current cycle.
- **Budgets are total cost (CTC).** The Budgets bulk column is **Monthly Cost Budget (CTC)** (a file with the old
  **Monthly Payroll Budget** header is still read); Compensation › Budgets shows "Monthly cost budget, CTC" and "Current
  monthly cost (CTC)". The cost is salary without the meal and accommodation minimum-wage top-ups plus the employer's
  PF (with admin and EDLI), ESI, LWF, gratuity, bonus and medical insurance, projected for the open payroll month as
  Cost and budget does (`budget_cost.go`). The offer budget check adds the hire's projected total (default salary
  structure) to the location's and flags "location cost budget (CTC)" when it goes over.
- **Retainer shows the retainer value only.** For employment type Retainer the calculation returns one **Retainer
  fee** (`r.fee`, the fixed gross earned) with no Basic, HRA or Special allowance and no fixed component lines; TDS
  under 194J, net pay and CTC are unchanged (`hrms pay-compare -against` on a copy: only basic/hra/sa/pfBase → 0 and fee
  added). Shown on the salary preview, offer breakup and letter (`{{breakup}}`), payslip, payroll sheet and register
  ("Retainer fee" column only in a month with a retainer), full salary register and employee file.
- Tests: `calc.TestVarDueAndCaps`, `calc.TestRetainerFeeOnly`, `api.TestVarPayoutCycleAndTarget`,
  `api.TestBudgetCheckTotalCost`. Manual steps: Part Y of [docs/manual_test.md](docs/manual_test.md).

### New joiners for insurer, EPF and ESIC; which report each role sees (test sheet #14 and #21, 18 Nov 2026)

- **Reports › New joiners** now carries what the medical insurer and the EPF/ESIC portals need, one row per person:
  father or spouse name, date of birth, gender, mobile, email, Aadhaar, PAN, UAN, Existing UAN?, ESIC number, bank
  name, account and IFSC, monthly gross, place, and the PF nominees (name, relation, date of birth, share %) and medical
  family members (name, relation, date of birth) in repeated columns (as many as the most anyone has). A **Joined in
  month** preset sets the dates to one month; **Download Excel** (every report now has it next to Download CSV) gives
  the file. Masking follows the viewer's role like every report: IDs by the Aadhaar and PAN mode, bank account and
  IFSC by the bank mode, the gross by the salary mode, nominee names and dates of birth by the medical mode; personal
  details are blank without the personal details right; your own row is always in full.
- **Access control › Roles and rights › Reports this role sees**: every report grouped as on the Reports page, with a
  tick each (Tick all / Untick all per group). An unticked report leaves the role's Reports page, and running or
  downloading it is refused on the server (403). Untouched, every role keeps what it saw before. A report still needs
  the rights it uses (marked "Rights do not allow" when the role's rights do not). HR Admin always sees every report.
  Role level only: personal rights do not grant or block a single report. New collection `role_report` (migration
  `020_report_access` ticks every report for every role); `GET /api/access/reports?role=`, `PUT /api/roles/{name}/reports`
  `{reports: {id: true|false}}` (audited); a new role copies the ticks of the role it is copied from.

### Role and Skill required, hiring IDs for every employee, offer letter at joining (test sheet #2, #3, #22, #26, 18 Nov 2026)

- **Role and Skill are required like the department** (`api/hirefix.go`): Import employees (both columns are now
  required in the file and in each row, active masters), the offer form (checked on the server whatever Form setup
  says), the joining details of an older offer that has none (HR sees Role and Skill as required fields there; Send to
  Payroll refuses without them; the Joining ID record gets them), and a job change (location, department, designation
  or grade) of an employee who has none yet: Employee master update needs the Role and Skill columns for that row,
  `PATCH /api/employees/{id}/job` needs `jobRole` and `skill`. Existing employees are not blocked otherwise. A transfer
  approved through Approvals is not checked. They pass offer → candidate → employee at joining as before.
- **Shortlisting, Offer and Joining IDs for every employee** (`employee.hire_no`): one number from the candidate series
  (`candidate_seq`), shown as SI-0042 · OI-0042 · JI-0042 on the employee file header and the Status timeline. A hire
  through Onboarding has the candidate's number; Import employees takes the next number of the series per employee
  (with a timeline entry "Hiring IDs: …"). Migration `018_hire_ids` gives one to every existing employee (candidate's
  number where there is a candidate, else the next numbers in joining-date order); it only adds fields and timeline
  rows, never changes the Employee ID, and runs again safely. `GET /api/employees/{id}` and `/timeline` return
  `hireIds {si, oi, ji, no, via}`.
- **Offer letter released at joining** (Onboarding › Form setup › Hiring › "Release the offer letter at"): offer
  release is the default; for the departments and employment types chosen (either is enough) the offer still goes
  through approval, is released (the answer is due as usual) and HR records Accepted, but the letter is held: not
  generated or shown (`letter` is empty, `letterHeld: true`) until **Mark joined** releases it automatically (audited
  "Offer letter released at joining", timeline entry). The rule in force at release is kept on the offer
  (`offer.letter_at`, `letter_released_at`); the offer screen shows the rule. Settings: `hiring.letterJoinDepts`,
  `hiring.letterJoinTypes` on `PUT /api/onboarding/settings`.
- The old "Mark not joined" (`POST /api/candidates/{id}/not-joined`) stays on the server but no button reaches it.

### All-inclusive gross from a month (owner, 26 Nov 2026; reworked the same evening)

From the month set in **Compensation › Salary structure › All-inclusive gross from month** (`company_setting.allin_from`,
"YYYY-MM", empty = off, no default; `PUT /api/comp/all-in`, right `pay.setup` Edit, audited; `GET /api/comp/all-in`
for any signed-in user), the monthly gross HR offers or sets (G) is the gross salary and also carries the employer's
statutory shares. `calc/allin.go`, `api/allin.go`.

- **Earnings = G.** G is split by the structure into Basic, HRA, Special allowance and, when G ≤ the bonus ceiling
  (and the type gets bonus), a **Statutory bonus** line (BonusPct of min(Basic, BonusCalc)); F + bonus = G
  (`calc.AllInFixed` finds F to the paisa). Order everywhere: Basic, HRA, Special allowance, Statutory bonus,
  Gross salary (= G). The bonus is inside G, never on top; not in the PF or ESI wage.
- **Deductions** (from the employee): employee PF, **employer PF (incl. EPS)**, employee ESI, **employer ESI**,
  **EDLI**, employee LWF, **employer LWF**, TDS and the rest, each worked out normally on G's split (PF wage and
  ceiling, ESI wage = G less the bonus, ESI when G ≤ the ESI ceiling, EDLI on Basic up to ₹15,000, LWF on G with its
  rates and caps). Net = G − deductions (+ variable pay and reimbursement). `Result.ErDed` holds the employer's shares;
  ErPF/Eps/ErEpf/ErESI/Edli/LwfEr keep their amounts, so ECR, ESI and LWF challans and the statutory reports are
  unchanged.
- **Company cost**: EPFO admin charges, gratuity, PL encashment and (as before) medical insurance.
  **CTC = G + admin + gratuity + PL encashment + medical + variable target + reimbursement.**
- **Outside G, on top**: variable pay = target × paid days ÷ days, automatically every month (a % target is % of
  G); HR payouts are refused for those months (Payroll › Variable pay, the Salary breakup bulk) and ignored if
  entered earlier; the full and final adds nothing. The Reimbursement is unchanged.
- **Part month**: G earned = G × paid days ÷ days, split the same way; deductions on the earned wages; no re-solving.
- **Retainer**: no PF, ESI, bonus or LWF, so nothing of that is deducted (fee = G, TDS as set). **PF opt-out/exempt**:
  no PF or EDLI deducted, no admin.
- **Months**: payroll months ≥ the setting, for everyone; earlier months and approved or published payrolls never
  change (a change that would alter one is refused).
- **Screens**: breakups (Compensation try-an-offer, Salary calculator, pay preview, offer, employee) show the salary
  lines, "Variable pay / Reimbursement (pro-rata, additional)", the deductions with the employer's shares, net, then
  the CTC build-up. Payslip: "Statutory bonus" after Special allowance, the four employer lines under deductions,
  only "EPFO admin charges" under employer contributions, a note with G. Payroll sheet and full register: "Statutory
  bonus" and "Gross salary (G)" after Special allowance, the employer PF/ESI/EDLI/LWF deducted before Total deductions
  (register keeps the remitted employer columns). Cost console: company contributions = admin, gratuity, PL
  encashment (new column), bonus, medical; employer PF/ESI/LWF are 0 there (paid by the employee).
- **TDS projection**: salary = G (with the bonus) + variable + taxable reimbursement; the employer's PF deducted from
  the employee counts as the employee's PF does (80C in the old regime); employer ESI, EDLI and LWF are ignored like
  the employee's ESI and LWF.
- Without the setting nothing changes (`hrms pay-compare -against` 0 differences on a live copy, Oct and Nov 2026).
  Tests: `calc.TestAllIn*`, `api.TestAllInGrossFromMonth`.

### Offer at joining (owner, 26 Nov 2026)

Lower-level hires (UT, for example) get no offer after the interview; the offer is given when they join. Any selected
candidate can take this path (HR picks; no grade or designation rule). `api/offer_at_joining.go`.

- **Choose.** Candidate › Offer › **Offer at joining** (right `onb.raise` Edit): expected joining date (today or later)
  and an optional note (`POST /api/candidates/{id}/offer-at-joining`; `candidate.oaj_at/oaj_by/oaj_doj/oaj_note`).
  Timeline "Offer at joining chosen", audited. The candidate sits in its own pipeline column **Offer at joining**
  ("waiting for the joining day"). Until an offer is raised HR can still press **Raise offer v1 instead** (clears the
  choice, timeline "Switched back to the usual offer") or **Close candidate**.
- **Raise offer at joining** (from the expected date; earlier is refused: use Reschedule joining): the same offer form,
  joining date prefilled; saved as offer v1 with `offer.at_joining` and sent through Offer approval exactly as usual.
  It is never released (Release is refused), never answered and never lapses (the hiring sweep skips it).
- **Mark joined** waits for that approval ("Waiting for offer approval"). After approval Mark joined records the
  acceptance in the same transaction: Offer ID issued, version frozen ("Accepted at joining"), the letter released at
  joining (`letter_at` "joining"), then the usual joining (JI- record, portal create, accommodation, machine gate).
- **Joining outcomes** before joining: Reschedule joining moves the expected date (and the offer's), No show starts the
  waiting window ("Came: mark joined" also needs the approved offer), Offer declined closes as today (an offer still
  waiting for approval is cancelled). Everything after joining is unchanged; the usual offer path is untouched.

### Half-day leave, short leave and the Reimbursement in fixed pay (owner, 20 Nov 2026)

- **Half-day leave end to end.** When the leave plan has "Half-day leave allowed", the employee (My leave) and HR on
  their behalf (Approvals › New leave request, "Half day": first or second half on leave) apply for half a day on any
  type the plan offers them; `payload.half` + `payload.halfOf`, one date, `facts.days` 0.5, so the balance falls by 0.5.
  Attendance shows "Casual leave (half day, first half) + Present": the other half is present when its hours reach the
  half-day hours of the day's shift (the work policy's "Half-day hours" without shift rules), else "+ Absent" with 0.5
  loss of pay; no punch follows the no-punch rule at half weight, one punch counts as present (as before). Only an
  unpaid type adds 0.5 LOP for the leave half. Opening balances (Bulk update and Import employees) already take .5.
- **Short leave (part of a day).** New option "Short leave" in the same dialogs: a date, from-time and to-time, reason.
  It is a leave request (`wf` leave, `payload.short`, `ltype` "Short leave", `fromTime`/`toTime`, `facts.days` 0, group
  C), so the leave approval flow, the approval hierarchy tick and HR's "Skip approval" apply; no balance moves. Leave
  plan settings (Policies › leave plan › Short leave): allowed (default on), how many a month (`lv.shortN`, default 2)
  and the most hours each (`lv.shortH`, default 2); beyond either, or on a weekly off / holiday, after the regularization
  window, the leave closure or a paid month, it is refused. Once approved the window is excused on the day
  (`attend.Plan.Short`): arriving after a window at the start is not late, leaving for one at the end is no early exit,
  its hours count as worked (no short-hours or half-day penalty), and the late / not-in alerts wait for its end. The day
  stays Present and shows "Short leave 2h". It is never a day of leave: month leave counts, the staffing limit and
  full-day overlap checks leave it out (a full-day leave over a short one is refused until it is cancelled). Report:
  Attendance and roster › Short leave.
- **Reimbursement in the fixed pay.** Every salary structure has a fixed earning **Reimbursement** (`calc.ReimbComponent`,
  method "Amount set on each employee", `per_emp`): ₹ a month entered on the employee (Employees › Edit pay options,
  Bulk update › Salary breakup column **Reimbursement (₹)**, Payroll › Salary calculator and the pay preview), stored
  in `employee_pay.comp_amts.reimb`. It is part of the fixed gross: Basic and HRA as the structure says, then the
  Reimbursement, and Special allowance takes the rest, so gross and CTC stay as entered (prorated by paid days like the
  rest). Flags: taxable, not PF, ESI or bonus wage (changeable in the component designer). An amount above what Basic
  and HRA leave is refused ("Reimbursement ₹… is more than what is left after Basic and HRA…"); a retainer has none.
  Shown on the breakups, payslip, payroll sheet (column only in a month with one), salary register ("Reimbursement
  (fixed pay)"; the old "Reimbursements" column is still the claims) and the employee file. Migration
  `024_reimbursement` adds it to the component library and to every structure before its balancing component, with
  nobody holding an amount: `hrms pay-compare -month 2026-10,2026-11 -dump` before and `-against` after on a copy of
  the live data gave **0 differences** (13 and 15 rows). The offer's Compensation card has "Reimbursement, ₹ a month"
  (`offer.data.reimb`, same refusal on check and raise, shown in the offer breakup and letter `{{breakup}}`), carried to
  `employee_pay.comp_amts.reimb` at Mark joined.
- Tests: `calc.TestReimbursementInFixedPay`, `api.TestReimbRoom`, `api.TestOfferReimbursementToEmployee`, `leave.TestShortLeaveRules`,
  `attend.TestShortLeaveExcusesLate`, `api.TestShortLeave`, `api.TestLeaveHalfDayBalanceAndLop`. Manual steps: Part AB
  of [docs/manual_test.md](docs/manual_test.md).
- **25 Nov 2026 changes (owner).**
  - **Retainer TDS 1%, editable.** `calc.DefaultStructure.TdsRate` is 1 (was 10); migration `030_retainer_tds_1pct`
    sets every structure still on 10 to 1 (audited). Editable per structure (Compensation › structure "Retainer TDS",
    "TDS, % of gross (editable)") and per employee (Employees › Edit pay options for a retainer, `employee_pay.tds_pct`,
    `PUT /api/employees/{id}/pay` with `tdsSet` + `tdsPct`, null = the structure's rate, 0–50, change history "TDS %").
    `calc.Result.tdsRate` carries the rate used; payslip/preview notes say "N% of gross" (no "section 194J" on pay
    screens; the Tax returns page keeps the 194J/26Q filing wording). The bulk salary template has no TDS column.
  - **Reimbursement outside the gross**, like variable pay (replaces the 20 Nov "inside the gross" rule): the Basic/HRA/
    Special allowance split is as if there were none; the amount is earned by the days paid (amount × earned gross ÷
    full gross), in no PF, ESI or bonus wage, and added to total earnings, net pay and CTC/cost (`Result.Reimb`,
    `CompLine.extra`). Any amount ≥ 0 is taken (the "what Basic and HRA leave" refusal is gone); none for a retainer.
    Shown separately on the offer and letter, Pay options, Salary breakup and calculator, payslip ("Outside the
    gross"), payroll sheet and register ("Reimbursement (outside the gross)" after Special allowance), Cost and budget
    (`costLine.reimb`, Excel column; offer cost includes it) and the TDS projection (`taxFig.reimb`; taxable unless the
    component says not).
  - **Realtime portal code = the ID's number, 6 digits** (JI-000006 → 000006, EMP-1054 → 001054; `ptIDCode`): the
    automatic create sends it as Employee code (card = it padded to the card width) and it becomes the Attendance code,
    so pulled punches match (leading zeros ignored). An ID without digits takes the next free number as before; a code
    another employee already has holds the create with a message.
  - **Employee codes by type from one series**: with one series for everyone (`by_type` off), a Retainer gets
    RET-<next> (the Retainer series' prefix) and everyone else EMP-<next> from the SAME counter and width, a number
    either prefix holds being skipped for both (`employeesPrefixes`; Employee ID step, Mark joined, import, next-code).
    A series per type behaves as before.
  - Tests: `calc.TestReimbursementOutsideGross`, `calc.TestRetainerTds`, `store.TestTdsOldDefaultToOne`,
    `api.TestRetainerTdsOverride`, `api.TestReimbCheck`, `api.TestPortalIDCode`, `api.TestPortalAutoCreate`,
    `api.TestEmployeeCodesRetEmpOneSeries`. Manual steps: Part AK of [docs/manual_test.md](docs/manual_test.md).

### Hiring IDs from the Employee code; blank nomination forms (owner, 24 Nov 2026)

- **Hiring IDs of imported staff come from the Employee code** (replaces the 18 Nov "next number of the series" for
  them): only the prefix changes, the digits stay exactly: EMP-000004 → SI-000004 · OI-000004 · JI-000004, EMP-1050 →
  SI-1050, a code of digits only (1050) → SI-1050 (the code's trailing digits; `employee.hire_no` the number,
  `employee.hire_digits` the digits). If a candidate (`candidate.no`) or another employee already has that number (two
  series, EMP-0005 and CON-0005), or the code has no digits, the person takes the next number of the candidate series
  as before and the timeline entry says why ("SI-0005 is already used by employee EMP-0005, so the next number…").
  Import employees raises `candidate_seq` past the numbers used, so a later shortlist never repeats one (after
  importing EMP-1050 the next shortlist is SI-1051 or later). Hires through Onboarding are unchanged (SI/OI/JI the
  candidate's number, the Employee code from its own series). `hireIds.via` is `candidate`, `code` or `import`.
  Migration `025_hire_ids_from_code` re-derives the IDs of every existing employee without a candidate record and
  rewrites their "Hiring IDs: …" timeline entry (the old IDs kept in its reason); hires via candidates are untouched;
  it runs again safely.
- **Blank form soft copy for a nomination document** (Policies › Nominations › Nomination policy › Edit, "Documents
  needed to enrol", column Blank form): HR Admin attaches a PDF, Word (.doc/.docx), JPG or PNG of up to 5 MB to a saved
  document row (e.g. Signed nomination form → Form 2.pdf), replaces or removes it at once (audited "Blank form
  attached / replaced / removed …"; removing marks the file replaced, nothing is deleted; a switched-off row keeps its
  form). The employee sees "Download blank form" next to that document in My pay › Nominations, fills it and uploads
  it as before. Stored as a company document (`owner_kind` company, `owner_id` `nomination/<id>`, `doc_key` the row's
  id; Google Drive folder Company, "Blank form Signed nomination form (Provident fund (EPF) nomination).pdf"). Download
  (`GET /api/files/{id}`): HR Admin, Payroll (`pay.claims`) and active employees the nomination applies to; anyone
  else 403. `POST` / `DELETE /api/nominations/schemes/{id}/blank/{doc}`; the schemes carry `docs[].blank`.
- Tests: `api.TestHireIDsFromCodeImport`, `api.TestHireIDsFromCodeMigration`, `api.TestNomBlankForm`. Manual steps:
  Part AC of [docs/manual_test.md](docs/manual_test.md).

### Leave earned by working days (owner, 24 Nov 2026)

- **Why:** with Upfront (or Monthly) credit an employee could take a whole year's leave at once. A leave plan now has
  a tab **Earn by working days** (Policies › Leave plans › a plan) with rules: **Paid days** (typed by the admin)
  and under it one or more rows **Leave type** (any type with a balance in the plan) + **Days earned** (halves
  allowed); Add rule, Add leave type, Remove. Nothing is filled in: every number is the admin's. Example an admin might
  enter: Paid days 7 → Casual leave 1, Privilege leave 1. Stored as `lv.earn` (`[{days, types:[{type, days}]}]`),
  saved with the plan (plan history "Earn by working days", audit). A leave type can be in only one rule ("… is
  already in the rule for every 7 paid days. A leave type can be in only one rule"); rules need paid days, at
  least one type and days earned (whole or half), and only balance types of the plan (`leave.EarnProblem`).
- **Meaning (owner, 24 Nov evening: "Working days means Paid days for leaves", credited as soon as the days are
  complete, "accrued only on active employee"):** every time the employee completes that many **paid days**, each
  listed type is credited its days earned **from the day after the Nth paid day**, not at the month end. Example:
  joined 15 Jan, a rule of 30 paid days, nothing lost: the 30th paid day is 13 Feb and the leave is there on 14 Feb.
  Whole blocks only; the leftover counts toward the next block. The count runs through the leave year (a block can
  span months) and starts again with the next leave year (balances carry forward as the type says; a block completed
  on the year's last day is credited the next day and counts in the old year's balance). A type in a rule is earned
  **only** this way: no monthly or upfront credit. Its Credit on the Leave types tab reads "By working days (see Earn
  by working days)"; **Days a year** becomes an optional cap (blank or 0: no cap; with an opening balance, the share
  of the year left).
- **Paid days** = paid days (present, half day 0.5, paid leave, weekly off and holiday; loss of pay days do not
  count): each day counts 1 less its loss of pay as the attendance engine settles it for payroll
  (`attend.DayOut.Paid`, replacing `DayOut.Worked`). Present, Late and Called to work 1; Half day 0.5; paid leave 1
  (half a day of paid leave plus the other half worked 1); Weekly off and Holiday 1 (worked or not); Absent, loss of
  pay leave and a No punch day once it has turned to loss of pay 0. A No punch day still inside the regularization
  window has no loss of pay yet, so it counts until it turns to loss of pay; a day beyond a deviation allowance counts
  by its loss of pay. Days before joining, after the last working day, today and later count 0. The month's late-mark
  half days (`Month.LateOver`, tied to no day) are not taken off, so a month's paid days for leave can be higher than
  payroll's by those half days.
- **When:** counted day by day from the type's accrual start (joining, or the day probation ends; all of it once
  probation is over when "Back-date at confirmation" is Yes, credited when probation ends) up to the day before the
  balance date, never today or later (`leave.Through`); a leave request is checked against the balance on its first
  day (for a future date: days over by today). Nothing is stored: balances are worked out live, so a regularization
  to present raises the count and a No punch day turning to loss of pay lowers it.
- **Only active employees earn:** status Active; someone on notice up to the last working day; a leaver (Inactive)
  up to the last working day (F&F encashment on the last working day counts that day once it has passed); an Inactive
  record without a last working day (an offer declined before joining) and a joiner not active yet (Pre-boarding)
  earn nothing. Months before the first punch or approved leave on record (from before the HRMS) are left out; the
  count starts on the 1st of that month.
- **Unchanged:** Monthly and Upfront types (and plans without rules) give exactly the same balances; employment types,
  carry forward, opening balances and the negative balance limit apply as before. No migration. A plan saved with the
  short-lived per-type "By days worked" (`credit: worked`, `perDays`) reads as its own rule (N paid days → 1 day,
  `leave.Normalize` when plans are loaded and saved).
- **Shown on:** the plan's type summaries and list, the policy's Leave lines and Leave simulator (type the paid days
  in a month, empty until typed; spread evenly over each month, it shows each credit's date in "Credited on"), My leave
  (earned so far, paid days counted up to yesterday, days to the next block, last credit date; open it for the paid
  days by month and each credit's date), the balance line in the leave dialogs, the employee file's Privilege leave
  line, the offer's leave text and the leave request rules. Every balance (My leave, leave checks, F&F encashment,
  leave report) goes through `leaveBalOf`. `GET /api/leave/balances/{id}` types carry `balance.worked` (`every`,
  `per`, `days`, `blocks`, `left`, `toNext`, `earned`, `cap`, `credit`, `through`, `credits` [{`on`, `blocks`,
  `credit`}], `months` [{`month`, `days`}]).
- Tests: `leave.TestEarnBlocks`, `leave.TestEarnCreditedTheDayAfter`, `leave.TestEarnStopsAtLastWorkingDay`,
  `leave.TestEarnAcrossMonthsAndYears`, `leave.TestEarnOnlyAndCap`, `leave.TestEarnRulesChecked`,
  `leave.TestEarnOldShape`, `leave.TestWorkedThrough`, `leave.TestEarnAfterProbation`, `leave.TestEarnCarryAndOpening`,
  `leave.TestWorkedLeavesOtherCreditsAlone`, `attend.TestPaidDays`, `api.TestLeaveByDaysWorked`. Manual steps: Part AE
  of [docs/manual_test.md](docs/manual_test.md).

### Deviation allowances (owner, 24 Nov 2026)

- **Where:** Policies › a work policy › Attendance › **Deviation allowances**: Early check-in, Late check-in and Early
  check-out, each with On/Off, **Minutes allowed** and **Times allowed in a month**. Every rule is off on every policy
  and both numbers are empty until the admin types them (no defaults); saving a rule that is On without both is
  refused ("Late check-in is on: enter the minutes allowed and the times allowed in a month, or switch it off"); 1–720
  minutes, 1–31 times. Stored as `settings.dev` (`{earlyIn, lateIn, earlyOut}`, each `{on, mins, n}`), with the
  policy's history and audit. For one person, give them their own rules from the roster (below), or map a policy to
  them (Applies to › Individual).
- **Late check-in** (later than the shift start + the late-mark grace, at most M minutes): on the first N such days of
  the calendar month the day is no late mark (Present, not counted in "Late marks allowed in a month" or its half
  day), note "Late check-in allowed, 1 h (2 of 5)". No hours are credited: the full-day hours still decide the day.
- **Early check-in** (earlier than the policy's early check-in allowance `rs.earlyIn` and at most M minutes before the
  start, so ordinary early arrivals do not use up the count): on the first N days the shift runs that much earlier,
  so hours count from the in punch whatever "Time worked before the planned start" says (08:00–16:00 on a 10:00–18:00
  shift is 8 h) and the end moves too.
- **Early check-out** (more than the early-out grace `att.early` and at most M minutes before the end, the slid end
  when the same day had an allowed early check-in): on the first N days a note only and no Early exit deviation. Only
  days that are over count (the last out punch of today may still change). No hours are credited.
- **Counting:** per employee and calendar month, in date order; only days within the minutes use up the count. A day
  of leave (full or half), a regularized day, a day called to work, a week off or a holiday is never a deviation day.
  Allowed days are not roster deviations (the Shift / Early exit kind is dropped: `attend.Allow.Trim`) and get no
  late-arrival alert. While any active policy has a rule on, the engine reads each month from its 1st (`loadMonth`).
- **Beyond the allowance** (the N+1th day, or more than M minutes): the rule's **Action** decides the day (next
  sections; No action = the rules exactly as before) and HR is told:
  a bell message to every active HR Admin and the employee's reporting manager, once per employee, date and rule
  (`notice.ref` `att-dev:<rule>:<date>:<employee>`), e.g. "Karan Khan (EMP-1020): late check-in 2 h 10 min on 24 Nov
  2026, 6th this month (allowed 5)". The sweep runs every 5 minutes on the job queue (`att_dev`), whatever the
  attendance alert settings say, and looks at today and the two days before, so switching a rule on does not flood the
  bell with the month so far. The day's note says "… : beyond the allowance".
- **Shown on:** the month register (Attendance › Records › one employee, My time › My attendance: Note column) and its
  counts ("Late check-in allowed", "Early check-in allowed", "Early check-out allowed", "Beyond the deviation
  allowance" in `month.counts`, shown as pills when there are any).
- Tests: `attend.TestDevLateIn`, `attend.TestDevEarlyIn`, `api.TestDevAllowLateIn`. Manual steps: Part AG of
  [docs/manual_test.md](docs/manual_test.md).

### Called to work on a holiday or leave day (owner, 24 Nov 2026)

- **Where:** Attendance › **Called to work** (HR views): the month's calls with the result, and **Call to work**: one or
  more employees, a date and a reason (required). The date must be a holiday on the person's calendar or a day of their
  approved leave (week offs are not covered yet; a working day is refused). Right: **att.records at Edit** (the right HR
  uses to change attendance, as for the MIS upload), within the caller's data scope; the list needs View. Audited; a
  leave request gets a history line "Called to work on … : <reason>". Refused in a payroll month already approved or
  published, and, like an MIS upload, in a month locked by payroll unless an exception is open. Removable (with a
  history line) on the same terms. `GET /api/attendance/called?ym=`, `POST /api/attendance/called`
  (`{employeeIds, date, reason}`), `DELETE /api/attendance/called/{id}`. Stored in `work_call` (migration 027).
- **Came** = the day has a punch (owner's rule "if they come then present": hours, a late arrival or a missing out
  punch do not matter; hours counted under the roster's shift that day, else the employee's own). Then the day is **Present** with the note "Called
  to work, worked 8:05" (`DayOut.called`): no loss of pay; on a leave day the date's leave is returned in every balance
  (`leaveBalOf` → `calledReturns`: 1 day, 0.5 for a half-day leave), the request stays approved and gets the line
  "Worked on a leave day … 1 day of Casual leave returned" (the 5-minute sweep, once); an unpaid (loss of pay) leave
  day loses its loss of pay; paid leave pays the same. Earn by working days counts the day 1, even on a holiday.
- **No punch**: the holiday or leave stays exactly as it was, with the note "Called to work, no punch: the holiday
  stands". A call never adds loss of pay.
- A call on a date that has since stopped being a holiday or leave day (roster changed, leave cancelled) does nothing
  ("No longer a holiday or leave day" in the list).
- Tests: `attend.TestCalledToWork`, `api.TestCalledToWork`. Manual steps: Part AG of
  [docs/manual_test.md](docs/manual_test.md).

### Individual roster changes: cell picker, called to work, other time, own deviation allowances (owner, 24 Nov 2026)

- **Where:** Shift and roster › Roster. A click on a cell opens a small picker instead of cycling the codes: each shift
  with its timing, WO, PH, **Other time for this day**, and on a holiday (PH on the employee's calendar) or a leave
  cell **Called to work** with a shift and a reason. The employee's name opens their **Deviation allowances**. Pattern,
  publish, alerts, deviations, discipline and the bulk upload work as before.
- **Called to work from the roster:** choosing a shift on a holiday or approved-leave cell calls the person to work with
  that shift: the same `work_call` record, engine, right (**att.records at Edit**), scope and refusals as Attendance ›
  Called to work (paid month; payroll-locked month without an exception), with the reason required. The shift is
  stored on the call (`work_call.shift_code`) and the day is measured against it (hours, late mark, the punch window).
  Came (any punch) = Present; on a leave day the leave comes back and an unpaid leave loses its loss of pay; no punch =
  the holiday or leave stays, no penalty; Earn by working days counts it 1. The cell shows the shift with a `*`
  (dashed border) and the tooltip "Called to work (…): G 09:00–18:00. <reason>. By …"; the On duty count counts it.
  "Remove the call" in the picker (`DELETE /api/attendance/called/{id}`) puts the holiday or leave back; another shift
  in the picker changes the call's shift. A roster change on a called day is refused until the call is removed. A
  plain shift on a calendar holiday (roster cell, roster upload) is now refused with "… is a holiday … A shift on a
  holiday calls them to work": it no longer makes an ordinary working day. Week offs work as before. A PH that came
  from a roster change (not the calendar) takes a shift as a plain roster change.
  `POST /api/roster/cell {employeeId, date, code: <shift>, call: true, reason}`.
- **Other time for this day (code OT):** a start and end with an optional second part, checked like a split shift in
  Masters › Shifts (second part after the first, same day; a day in one part may end after midnight and is then
  recorded like a night shift). No shift master is needed: the times are stored on the roster entry
  (`roster_entry.start_time/end_time/start2_time/end2_time`, the log's `from_time/to_time`). The engine treats the day
  exactly like a shift with those timings: hours (the split gap never counts), late mark from the first part's start,
  the punch window, deviation allowances; attendance rules are the work policy's (no shift master, no shift rules).
  It obeys the roster's lock of past dates, the late-change reason and the rest rules like any shift change, shows as
  "OT" with the times in the tooltip and an "Other time" row in the footer, and counts in On duty. OT is now a roster
  code: no shift master can take it (none had it). `POST /api/roster/cell {employeeId, date, code: "OT", time: {start,
  end, start2?, end2?}, why}`.
- **Deviation allowances for one employee:** Early check-in, Late check-in and Early check-out each **Use the policy's
  rule** (default), **On** with Minutes allowed + Times allowed in a month (both empty until typed; On refused without
  both; 1–720 minutes, 1–31 times, as on the policy) or **Off for this employee**, with an **Applies from** date and an
  optional note. Kept as versions (`emp_dev_allow`, migration 028; same date replaces), audited; a date in an approved
  or published payroll month is refused. The engine uses, for each day, the version in effect laid over the policy's
  rules (`attend.DevIn.Rules`); a rule's count runs on through the calendar month. Notes, counts and the bell alerts
  follow. Right to change: **pol.policies at Edit** (the right that edits work policies' allowances: giving one person
  an allowance must not need less than giving it to a policy); view: pol.policies, ros.roster or att.records at View;
  both within the caller's scope. `GET/POST /api/attendance/dev-allow/{id}`. The roster row says "own deviation rules"
  when the employee has their own in effect.
- Tests: `api.TestRosterCalledToWork`, `api.TestRosterOtherTime`, `api.TestDevOwnRules`. Manual steps: Part AH of
  [docs/manual_test.md](docs/manual_test.md).

### Deviation allowances: the Action beyond the allowance (owner, 24 Nov 2026)

- **Where:** the **Action** column of Policies › a work policy › Attendance › Deviation allowances, and of the
  employee's own rules (roster › click the name; "Use the policy's rule" takes the policy's action too). Choices: **No
  action**, **Absent**, **Half day until regularized**, **Present until regularized**. No default: the dropdown says
  "Select" until the admin picks; a rule saved On without minutes, times and an action is refused ("Late check-in is
  on: choose the action for a day beyond the allowance, or switch it off"). Stored as `act` (`none`, `absent`, `half`,
  `present`) on `settings.dev.<rule>` and on `emp_dev_allow.rules.<rule>` (with `use: on`). A rule saved On before the
  Action existed reads as No action, so nothing changes (hrms had no rule saved on 24 Nov 2026; no migration).
- **What a day beyond the allowance gets:** No action: the usual rules (late mark, "Late marks allowed in a month",
  hours). **Absent**: "Absent until regularized", loss of pay 1. **Half day until regularized**: loss of pay 0.5 (as the
  missing-punch option of that name). **Present until regularized**: no loss of pay, flagged. With these three the
  action replaces the day's late mark (no late mark, not counted in the late-mark limit: no double penalty). The hours
  rules still apply: when they give more loss of pay (5 h late and 3 h worked is Absent (short hours) even with Half
  day), the hours stand, with the larger loss of pay, never both. Two rules beyond on one day: the harsher action wins
  (Absent > Half day > Present > No action).
- **Regularizing:** such a day carries `reg` (the action) in the month register; My time › My attendance shows
  **Regularize** on it and Attendance › Issues lists it. The usual regularization flow applies (employee request →
  approval; the window `rr.back`, the payroll cycle cut-off, the monthly limit, exceptions). Once approved the date is
  a regularized day: never a deviation day, settled by the corrected times like any regularized day.
- **Display:** the note says "Late check-in 2 h 10 min, more than the 2 h allowed: beyond the allowance → Half day
  until regularized" (plus "the hours or punches give more loss of pay: …" when they decide); counts "Absent / Half day / Present
  until regularized" (days beyond with that action) beside "Beyond the deviation allowance"; the absent days count with
  No punch, the half days with Half day, the present days with Present (payroll register). The bell ends with "→ Half
  day until regularized". The payroll cut-off reminder includes Half day until regularized days.
- Tests: `attend.TestDevAction`, `api.TestDevAllowAction`. Manual steps: Part AI of
  [docs/manual_test.md](docs/manual_test.md).

## Built so far (spec 22.1)

Sprint 1 and 2 foundations: sign-in with OTP, role mapping with one-holder-per-place, rights and masking, server-side
scope, masters with inactive-with-date-and-reason (no delete), employees (scoped list, masked detail, job changes with
reason and effective date, scheduled changes, change history, mark inactive, employee codes, import), append-only
audit log, the approval engine with all seeded flows, inbox, simulator and flow editor, SLA escalation,
ledger finalizers for claims, arrears, deductions, advances and bank changes, River job queue, salary calculator,
dashboard.

Sprint 3: onboarding (offer form driven by a configurable field and document setup, live breakup, pay band and
budget check, offer approval through the same engine, pre-boarding employee with the next code, offer letter from a
template, release, accept or decline, joining details, Payroll verification, active on the joining date, rejoin),
documents (stored under `STORAGE_DIR`, default `backend/data/uploads`; never deleted, a new upload replaces the old),
bulk update with Excel templates and drop-downs (employee master, personal details, mark inactive, salary breakup, gross revision with
automatic arrears or recoveries, pay bands, budgets), nominations (policy, employee submission with documents, Payroll
verification and overdue tracking), company details.

Sprint 4: work policies (every setting of spec 10.1, mapped by employee, location, cluster, department, designation,
grade, type, city or state, with history), leave plans and types, holiday calendars by city, exceptions, the weekly
roster (policy defaults, late changes with reasons, locked past dates, rest-gap check, publishing, roster upload),
attendance (upload, punch rules, daily status, month register with loss of pay, missing punches, date view),
regularization and leave rules on the approval engine (balances, windows, blackouts, staffing limit), deviations with
OK / Not OK (OK regularizes the roster), discipline scores, warning letters and improvement plans. The rules live in
`internal/policy`, `internal/attend` and `internal/leave` as pure functions with tests from the reference cases.

Run `make seed` once after upgrading: it adds the five work policies, three leave plans and city calendars, and six
weeks of sample punches for the sample organisation.

Sprint 5: payroll. Compensation (structure versions with every rate editable, mapping people to a version from a date
with a mid-month blend, pay bands with copy between cities, labour welfare fund by state, property budgets), the
monthly cycle (month close with pending requests by approver, reminders, attendance summary, lock or reject-and-lock;
pilot group; dry run with flags, filters, paging, per-row edits and a corrected-sheet upload; run checks; verify,
approve and publish by separate rights; large runs on a River job), payslips (setup with logo, accent and codes;
snapshot at publish; print or save as PDF; none for retainer and contractual staff), bank advice, register and cost
by city, department, type and grade, pay items through the approval engine (advances with recovery schedules, skip
next and recover balance; deductions with the Payment of Wages Act check; arrears; claims with the bill, closing 15
days after the last working day), salary holds with release, upcoming pay lines, carry-over of negative nets, the
employee file (readiness for the month), the night allowance from the work policy (overtime and comp-off credits were
removed on 8 Nov 2026), leave clubbing and team cover. The open payroll month is the month after the last approved
run (last month before any run; Payroll settings can set the first month). Approve and publish need `pay.approve`
(Payroll Manager, CHRO, HR Admin).

Sprint 6: income tax and statutory files. Declaration and proof windows per financial year (dates, or forced open or
closed), the employee's declaration (regime, every 80C to 24(b) line with its cap, rent with landlord name and PAN,
other and previous-employer income), proof upload, Payroll review (approve an amount or reject with a reason),
finalisation after the proof window (only approved amounts count; the difference is spread by the monthly TDS),
monthly Section 192 TDS in the payroll run (the year's tax for the months employed, less TDS so far and the previous
employer's, over the months left; frozen once published), retainer TDS under 194J, TDS deposits with BSR and challan
and an overdue flag, 24Q and 26Q quarterly data with receipt numbers, Form 16 Part A and Part B (provisional during
the year, final after March and the Q4 filing; released by a payroll approver; needs the company TAN and PAN; print all
in one file), and the statutory files: insurance, PF and ESI enrolment and exit over a date range (skipping people
already sent), ECR (Excel and the #~# text file) and ESI contribution from an approved month.

Sprint 7: separation and employee dues. One case per exit (resignation, termination, absconding, contract end,
retirement, death) through the Separation approval: an employee starts only a resignation (its last working day is the
date given plus the policy notice); a current approver may bring it earlier and waive or recover the shortfall (a
"Notice period shortfall" deduction); withdrawal within the policy window; termination with pay in lieu, serve or no
notice; absconding with two show-cause letters three days apart, an automatic salary hold and "reported back".
Clearance by the reporting manager, HRBP and Finance with configurable items, two of them automatic (assets returned,
exit interview); the private exit interview; letters (show-cause, termination, relieving, experience, no dues) to print
or save as PDF; closing with the rehire decision. Full and final: leave encashment, gratuity, notice pay or recovery,
recoveries and earnings scheduled after the last working day brought forward (and voided on approval), carry-overs,
held salary for information; approved by net amount through p_fnf, paid within 45 days. Inventory: a central store and
one per HRBP, additions and transfers (HRBP Corporate), issues and returns (HRBP), serial-numbered assets that are never
deleted, lost assets with optional recovery, uniform recovered 50% / 50% and refunded after 6 months.

Sprint 8: reports, dashboards, scale and the punch connector. 35 reports in six groups (spec 20), each within the
caller's data scope, with the prototype's columns and filters, an on-screen preview and a CSV download that needs `rep.reports` at Edit and the
role's download right (downloads are audited). Dashboard Payroll tab (runs, cost by city, statutory totals, holds,
waiting items) and Compliance follow-ups (TDS deposits past due, approved months not published). Attendance days, raw
punches and payroll rows are split into yearly partitions (2020-2035, extended by the maintenance job), with indexes for
growing lists; payroll runs above 2,000 people go to a River job. Punch machines: each device has its own key (shown
once, stored hashed) and pushes to `POST /api/punches`; punches are kept raw and each day's in and out are worked out
from them, never overwriting a regularized day; the MIS upload stays as the fallback.

Run `make api` again after upgrading: migrations 006 to 009 apply on start (009 moves attendance, punches and payroll
rows into partitioned tables, keeping every row).

UI matched to the prototype (4 Nov): every page, tab, dialog and role view now uses the prototype's style sheet and
markup, and the server returns what those screens show (report and statutory columns, approval health and PIP flows,
resignation change and withdraw, master order, role rights). Run `make api` again: migrations 011 to 021 apply on start.

The build plan for sprints 5 to 8 is in [docs/build-plan.html](docs/build-plan.html), and the results of the deep
test pass (automated, browser and 5,000-employee load) in [docs/test-report.html](docs/test-report.html). Browser and
load scripts: [tests/e2e](tests/e2e/README.md).
