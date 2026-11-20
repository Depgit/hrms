# Browser and load tests

These run against a running API and web app, on a **throwaway MongoDB database** (they create offers, payroll runs,
separations and more). Never point them at real data: always set `MONGO_DB` (the default is the live `hrms`
database) and `STORAGE_DIR` (`backend/.env` would otherwise send uploads to `data/uploads`).

```sh
cd tests/e2e && npm install
# from backend/: build once, then a fresh test database seeded with the sample organisation
go build -o /tmp/hrms-e2e ./cmd/hrms
export MONGO_URL='mongodb://localhost:27017/?replicaSet=rs0&directConnection=true' MONGO_DB=hrms_e2e
mongosh --quiet "$MONGO_URL" --eval 'db.getSiblingDB("hrms_e2e").dropDatabase()'
/tmp/hrms-e2e migrate
/tmp/hrms-e2e seed -sample
# API with on-screen OTPs (same MONGO_URL / MONGO_DB), and the web app pointed at it
PORT=8083 OTP_DEV_ECHO=true STORAGE_DIR=/tmp/hrms-e2e-uploads /tmp/hrms-e2e serve
API_URL=http://localhost:8083 npx vite --port 5198                                       # from frontend/

HRMS_WEB=http://localhost:5198 npm run roles   # every role, every page and tab
HRMS_WEB=http://localhost:5198 npm run forms   # 80 form flows (the joiner's link, and the payroll checklist's Mark as LOP half day and Hold salary among them); reseed before running again
HRMS_WEB=http://localhost:5198 npm run verify  # Payroll's joining verification and bulk approval; reseed first
HRMS_WEB=http://localhost:5198 npm run advances # an employee's own advance through approval, opening advances upload; reseed first
HRMS_WEB=http://localhost:5198 npm run search   # the people picker: typo, initials, keyboard, phone width (only reads)
HRMS_WEB=http://localhost:5198 npm run uniform  # uniform sizes on the record, issue by size, sizes summary, refund basis; reseed first
HRMS_WEB=http://localhost:5198 npm run bulkopen # Bulk update picker, Opening uniform and Opening advances (check, apply, Uniform card); reseed first
```

Reseed = drop the database, `migrate`, `seed -sample` again (the API can keep running). Without a local `mongosh`,
use `docker exec hrms-mongo-1 mongosh --quiet --eval 'db.getSiblingDB("hrms_e2e").dropDatabase()'`.
`npm run forms` rewrites `forms-results.json` with the run's results. `CHROME` sets the Chrome binary (default: the
macOS path).

Load test: `load.sql` (5,000 extra employees with a month of punches) is still written for the old PostgreSQL schema
and has not been ported to MongoDB yet. Until it is, load the data into a scratch Postgres copy of a seeded sample
organisation, run `psql -f load.sql` there, copy it into a separate throwaway Mongo database with
`MONGO_DB=hrms_load_e2e /tmp/hrms-e2e import-pg -from postgres://…/<scratch> -replace`, start the API on that
database and run `HRMS_API=http://localhost:8084 npm run load`.
