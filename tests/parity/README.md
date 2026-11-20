# Parity harness: the MongoDB build must answer every API call like the Postgres build

The port from PostgreSQL to MongoDB promises **zero behaviour change**: same routes, JSON keys and values, status
codes, error messages and ordering. This harness proves it by running the same script against both builds on the
same data and comparing every response.

- `capture.mjs` signs in one user for every role (HR Admin, Employee, Line Manager, Property Manager, Cluster
  Manager, City Head, Operations Head, HRBP, HRBP Corporate, Payroll, Payroll Manager, Finance Head, Function Head,
  CHRO, and the two roles nobody holds in the sample organisation — Finance and MIS Executive — which it maps to
  EMP-1047 and EMP-1048 through Role mapping), then
  1. calls **every GET route** of the API for every role (`get-before`),
  2. runs the **write script** (`writes.mjs`): masters, leave, regularization, claim, personal details change,
     transfer, consequence, roster, punch machine, deviations, onboarding SI → OI → JI → Employee ID, inventory,
     advance / deduction / arrear / salary hold, bulk upload, the payroll run with its checklist (dry run, edit,
     reopen, verify, approve, publish), company policy acknowledgement, nominations, tax declaration and proof,
     resignation with clearance and exit interview, termination, absconding, a withdrawn resignation — with the
     request bodies of the Go integration tests, wrong calls included (403/400 answers are compared too),
  3. calls every GET route again for every role (`get-after`).

  Every request and response is written, in order, to `<out>/records.jsonl` (`<out>/meta.json` has the summary).
  The GET routes come from `routes.mjs`; capture refuses to run when the route table in `backend/internal/api`
  has a GET route that `routes.mjs` does not cover, except `/api/mdm/*` (the CMS integration exists only in the
  MongoDB build, so there is nothing to compare it with; it is tested in `backend/internal/mdm`) and `/api/join/*` (the
  new joiner's public pages, which need a link made during a run; tested in `backend/internal/api/joining_link_test.go`
  and `tests/e2e/forms.mjs`). `GET /api/joiner-link/{id}` and the `filledBy` keys of Form setup, and `joinerLink` on
  the pipeline rows, are new since 11 Nov 2026 and show as differences against a Postgres capture. So are, since 12 Nov 2026,
  `GET /api/me/advance`, `GET /api/advances/settings` and `GET /api/advances/opening/template` (salary advances asked
  for by employees and opening advances, `advances.go`: 404 in the Postgres build), and the write `pay.adv-by-employee`
  (an employee's own advance, now allowed: 400 for its 13 instalments where the Postgres build answers 403). Also since 12 Nov 2026,
  `GET /api/me/directory/people` and `GET /api/me/directory/departments` (My view's company directory, `mydirectory.go`:
  404 in the Postgres build), and `GET /api/me/directory`, which takes `?dept=` and adds `id`, `mine` and each person's
  `ref` and `email`. Since 13 Nov 2026, `GET /api/inventory/sizes`, `GET /api/inventory/sizes/sheet` and
  `GET /api/employees/{id}/uniform` (uniform sizes, `uniform.go`: 404 in the Postgres build); Inventory setup, the uniform
  issues, Form setup (the Uniform section), `GET /api/me/dashboard?tab=pay` (`uniform`), the bulk templates (`uni`, and
  optional size columns on `new`) and the employee details download (a ninth group) gain keys; and a full and final no
  longer brings forward a uniform refund due after the exit (an info line `unirefund` instead of `earn2`). Also since 13 Nov 2026, the bulk templates
  `openuni` and `openadv` (Opening uniform and Opening advances, `bulk_opening.go`) are new in `GET /api/bulk` (with a
  `note` key) and in `GET /api/bulk/{tpl}/download`, and the `lvopen` hint and "How to fill" sheet changed; the uniform
  issue rows of an opening issue gain `opening`, `note` and `issuedYm` (none in the sample data). Since 14 Nov 2026,
  `GET /api/payroll/{ym}/icici-upload`, `GET /api/payroll/{ym}/icici-upload.xlsx` and `GET /api/payroll/{ym}/full-register.xlsx`
  (ICICI's bulk payment upload and the full salary register, `bankfile.go`: 404 in the Postgres build), and
  `GET /api/payroll/schedule` gains `iciciDebitAcc`. Path parameters are real ids from the list responses
  (HR Admin sweeps first); query variants are the ones the web app sends.
- `diff.mjs` compares two captures call by call (see *Reading the diff*). Exit code 0 only when every call is
  identical.
- `lib/normalize.mjs` — what may differ between runs (below). `rules.mjs` — the few places the Postgres build
  itself is not deterministic. `lib/xlsx.mjs` — Excel downloads are compared cell by cell.
- `golden.sh` — a capture from the original Postgres binary; `mongo.sh` — a capture from the MongoDB build.

Node 20+ only, no packages. Port 8092 is the harness's port.

## The data

Both builds run on the same data: the Postgres database `hrms_parity_src`, made once with the **original** binary,
and dumped. Never write to it; every run works on a fresh copy.

```sh
SP=/private/tmp/claude-501/-Users-batman-Documents-backend-HRMS/3d259efb-6a68-4747-b5db-631853d60e6d/scratchpad
docker exec hrms-postgres-1 createdb -U hrms hrms_parity_src
(cd $SP/pg-baseline/backend && DATABASE_URL=postgres://hrms:hrms@localhost:5433/hrms_parity_src ../hrms-pg migrate \
  && DATABASE_URL=postgres://hrms:hrms@localhost:5433/hrms_parity_src ../hrms-pg seed -sample)
docker exec hrms-postgres-1 pg_dump -U hrms -Fc hrms_parity_src > $SP/parity/hrms_parity_src.dump
# the uuids that exist before any run: kept as they are by the normalizer, every other uuid is "new"
docker exec hrms-postgres-1 pg_dump -U hrms --data-only hrms_parity_src \
  | /usr/bin/grep -oiE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | tr A-F a-f | sort -u > $SP/parity/known-ids.txt
```

If `hrms_parity_src` is gone, restore it from the dump (the ids must stay those of `known-ids.txt`):
`docker exec -i hrms-postgres-1 sh -c 'createdb -U hrms hrms_parity_src && pg_restore -U hrms -d hrms_parity_src' < $SP/parity/hrms_parity_src.dump`

The sample data is relative to the day it was seeded (six weeks of punches up to that day; payroll month October
2026 open). A later day still works — the write script takes its dates from today and from the responses — but
both captures that are compared must be made **on the same day**, and preferably one right after the other:
attendance for today, "late change" rules and the like depend on the clock. Making a golden capture takes about a
minute, so make a fresh one right before each MongoDB comparison.

## Golden capture (original Postgres build)

```sh
tests/parity/golden.sh $SP/parity/golden
```

which is: `createdb -T hrms_parity_src hrms_parity_run`; the original binary serving it
(`cd $SP/pg-baseline/backend && DATABASE_URL=…/hrms_parity_run PORT=8092 OTP_DEV_ECHO=true STORAGE_DIR=… ../hrms-pg serve`);
`node tests/parity/capture.mjs --base http://localhost:8092 --out <dir> --known-ids $SP/parity/known-ids.txt --api-dir $SP/pg-baseline/backend/internal/api`;
then the server stopped and `hrms_parity_run` dropped.

Two golden captures of the Postgres build compare identical (`node tests/parity/diff.mjs <one> <other>` exits 0):
that is how the normalizer and `rules.mjs` were tuned. Run that check again after changing the harness.

## MongoDB comparison

```sh
cd backend && go build -o $SP/hrms-mongo ./cmd/hrms && cd ..
tests/parity/golden.sh $SP/parity/golden                         # same day, right before
MONGO_BIN=$SP/hrms-mongo tests/parity/mongo.sh $SP/parity/mongo
node tests/parity/diff.mjs $SP/parity/golden $SP/parity/mongo > $SP/parity/mongo-diff.txt; tail -40 $SP/parity/mongo-diff.txt
```

`mongo.sh` is, step by step:

```sh
docker exec hrms-mongo-1 mongosh --quiet --eval "db.getSiblingDB('hrms_parity').dropDatabase()"
docker exec hrms-postgres-1 createdb -U hrms -T hrms_parity_src hrms_parity_run
cd backend
MONGO_URL='mongodb://localhost:27017/?replicaSet=rs0&directConnection=true' MONGO_DB=hrms_parity \
  $SP/hrms-mongo import-pg -from postgres://hrms:hrms@localhost:5433/hrms_parity_run -replace
docker exec hrms-postgres-1 dropdb -U hrms hrms_parity_run
MONGO_URL='mongodb://localhost:27017/?replicaSet=rs0&directConnection=true' MONGO_DB=hrms_parity PORT=8092 \
  OTP_DEV_ECHO=true STORAGE_DIR=$SP/parity/uploads-mongo $SP/hrms-mongo serve &        # stop it afterwards
node ../tests/parity/capture.mjs --base http://localhost:8092 --out $SP/parity/mongo \
  --known-ids $SP/parity/known-ids.txt --api-dir internal/api
```

Never use the Mongo database `hrms` (reserved for the user's data). The import must keep ids, codes and sequence
positions (`LV-3001` follows the dump's `approval_request_seq`), or every later call differs.

## What is normalized

- ISO date-times → `<ts+05:30>` (the zone is kept, so a value written in UTC shows up as `<tsZ>`). Postgres
  writes microseconds and MongoDB milliseconds, so stored times can only be compared this way.
- Date-times formatted for people (`11 Nov 2026 12:37`, `11 Nov, 12:37`, `2026-11-11 12:37`) → `<now>`, but only
  when they fall inside the capture's own run window (meta.json `started`–`finished`, a minute either side). Any other
  one is data and is compared. Plain dates `YYYY-MM-DD` are compared as they are.
- uuids in `known-ids.txt` are kept. Any other uuid was created during the run and becomes `<new:R.n>`: R is a
  short hash of the call it first appeared in, n counts within that call (in a canonical order, so the same
  elements in another array order get the same labels). Ids that are not uuids (`EMP-1054`, `LV-3001`, `SEP-1`,
  serial numbers) are deterministic and compared as they are.
- Session tokens, the dev OTP, device keys and their hint → placeholders.

`rules.mjs` lists the places where the original Postgres build is itself not deterministic (an array built by
ranging over a Go map, an unstable sort); those arrays are compared as multisets. Every rule says why. Do not
add a rule to hide a Postgres/MongoDB difference.

## Reading the diff

```
[status 200 → 500] write | leave.apply | POST /api/approvals
    missing  $.id: "LV-3001"
[different] get-after | Payroll | GET /api/payroll/2026-10/rows?size=500
    changed  $.rows[3].net: 18234.5 → 18234.49
    null-vs-missing $.rows[3].hold: null → (missing)
[order] get-after | HR Admin | GET /api/employees?size=500
    order    $.rows: 54 items → first out of place at [12]
```

Each call key is `phase | role (sweeps) or step (scripted calls) | METHOD path`; a key made twice gets `#2`.
Categories: `identical`, `order` (same content, another array order — a finding: the API promises the same order),
`different` (path-level: `missing` = only in the golden capture, `extra` = only in the actual one,
`null-vs-missing`, `changed`, `length`), `status`, `only-golden` / `only-actual` (the write script took another
branch, usually because an earlier call failed). Summary counts by phase and by role or step group come last.
Options: `--details N` (lines per call, default 12), `--only <regex>` (keys), `--json <file>`, `--quiet`.

A write that fails on the MongoDB build changes everything after it; read the diff top-down and fix the first
`status` or `different` in the `write` phase first.

## Size

About 18,600 calls per capture (≈45 s against the Postgres build): 67 sign-in calls, ~540 GET URLs per role ×
16 roles before the writes, ~300 scripted calls, ~600 GET URLs per role after them. `records.jsonl` is ~26 MB.
