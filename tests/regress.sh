#!/bin/sh
# Full regression run for the HRMS: `make regress` (everything) or `tests/regress.sh go parity e2e` (pick stages).
#
#   go      build, vet, gofmt, frontend type check, and every Go test against MongoDB (incl. the Postgres importer)
#   parity  the MongoDB build against the original PostgreSQL build, call for call on identical data (tests/parity)
#   e2e     browser tests: every role and page (roles), the form flows (forms), Payroll verification (verify) and salary
#           advances (advances), documents from existing employees (docs), uniform sizes (uniform) and the opening
#           uniform and advances templates of Bulk update (bulkopen), each on a fresh
#           sample seed, plus the people picker's search (search, on the roles seed)
#
# It never touches your data: tests use their own databases (Mongo hrms_test_*, hrms_parity, hrms_regress_e2e;
# Postgres hrms_parity_src/run, hrms_regress_pg) and ports 8092, 8095 and 5195 — stop anything on those first,
# or move the browser tests with E2E_API_PORT=8094 E2E_WEB_PORT=5194.
# The PostgreSQL baseline (the app as it was before the MongoDB move) is rebuilt once from
# backups/hrms-postgres-version-2026-11-11.tar.gz into .regress/ and reused.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=${REGRESS_DIR:-$ROOT/.regress}
MONGO_URL=${MONGO_URL_TEST:-mongodb://localhost:27017/?replicaSet=rs0&directConnection=true}
PG=postgres://hrms:hrms@localhost:5433
STAGES=${*:-go parity e2e}
API_PORT=${E2E_API_PORT:-8095}
WEB_PORT=${E2E_WEB_PORT:-5195}
mkdir -p "$WORK"
RESULTS=""
note() { RESULTS="$RESULTS\n$1"; printf '\n=== %s\n' "$1"; }
pgx() { docker exec hrms-postgres-1 "$@"; }
mongo_drop() { docker exec hrms-mongo-1 mongosh --quiet --eval "db.getSiblingDB('$1').dropDatabase()" >/dev/null; }
port_free() { ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1 || { echo "port $1 is in use: stop it first" >&2; return 1; }; }
wait_http() { i=0; until curl -sf "$1" >/dev/null; do i=$((i+1)); [ $i -gt 120 ] && return 1; sleep 0.5; done; }

docker compose -f "$ROOT/docker-compose.yml" up -d --wait mongo postgres >/dev/null || { echo "docker services did not start" >&2; exit 1; }
(cd "$ROOT/backend" && go build -o "$WORK/hrms" ./cmd/hrms) || { echo "build failed" >&2; exit 1; }

stage_go() {
  cd "$ROOT/backend" || return 1
  ok=0
  go vet ./... || ok=1
  [ -z "$(gofmt -l .)" ] || { echo "gofmt: $(gofmt -l .)"; ok=1; }
  (cd "$ROOT/frontend" && npx tsc -p .) || ok=1
  # The importer tests read a real PostgreSQL organisation: the original build's schema and sample data.
  pg_binary || { echo "could not build the PostgreSQL baseline" >&2; return 1; }
  pgx dropdb -U hrms --if-exists hrms_regress_pg; pgx createdb -U hrms hrms_regress_pg
  (cd "$WORK/pg-baseline/backend" && DATABASE_URL="$PG/hrms_regress_pg" ../hrms-pg migrate && DATABASE_URL="$PG/hrms_regress_pg" ../hrms-pg seed -sample) >/dev/null || ok=1
  HRMS_TEST_MONGO_URL="$MONGO_URL" HRMS_TEST_PG_URL="$PG/hrms_regress_pg" go test -count=1 -timeout 40m ./... || ok=1
  pgx dropdb -U hrms --if-exists hrms_regress_pg
  return $ok
}

# The original PostgreSQL build (the app before the MongoDB move).
pg_binary() {
  [ -x "$WORK/pg-baseline/hrms-pg" ] && return 0
  rm -rf "$WORK/pg-baseline"; mkdir -p "$WORK/pg-baseline"
  tar -xzf "$ROOT/backups/hrms-postgres-version-2026-11-11.tar.gz" -C "$WORK/pg-baseline" ./backend || return 1
  (cd "$WORK/pg-baseline/backend" && go build -o ../hrms-pg ./cmd/hrms) || return 1
}

# That build, its seeded source database and the ids that exist before the run.
pg_baseline() {
  pg_binary || return 1
  if [ -z "$(pgx psql -U hrms -Atc "select 1 from pg_database where datname='hrms_parity_src'")" ]; then
    pgx createdb -U hrms hrms_parity_src || return 1
    (cd "$WORK/pg-baseline/backend" && DATABASE_URL="$PG/hrms_parity_src" ../hrms-pg migrate && DATABASE_URL="$PG/hrms_parity_src" ../hrms-pg seed -sample) >/dev/null || return 1
  fi
  mkdir -p "$WORK/parity"
  pgx pg_dump -U hrms --data-only hrms_parity_src | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | sort -u >"$WORK/parity/known-ids.txt"
}

stage_parity() {
  port_free 8092 || return 1
  pg_baseline || { echo "could not prepare the PostgreSQL baseline" >&2; return 1; }
  rm -rf "$WORK/parity/golden" "$WORK/parity/mongo"
  export PG_BIN="$WORK/pg-baseline/hrms-pg" PG_SRC="$WORK/pg-baseline/backend" PARITY_DIR="$WORK/parity"
  # The golden is made right before the comparison: the sample data and several rules depend on today's date.
  sh "$ROOT/tests/parity/golden.sh" "$WORK/parity/golden" || return 1
  MONGO_BIN="$WORK/hrms" MONGO_URL="$MONGO_URL" sh "$ROOT/tests/parity/mongo.sh" "$WORK/parity/mongo" || return 1
  mongo_drop hrms_parity
  node "$ROOT/tests/parity/diff.mjs" "$WORK/parity/golden" "$WORK/parity/mongo" --json "$WORK/parity/diff.json" | tee "$WORK/parity/diff.txt" | tail -25
}

# A fresh sample organisation in hrms_regress_e2e, the API on $API_PORT (8095) and the web app on $WEB_PORT (5195).
e2e_up() {
  mongo_drop hrms_regress_e2e
  (cd "$ROOT/backend" && MONGO_URL="$MONGO_URL" MONGO_DB=hrms_regress_e2e "$WORK/hrms" migrate && MONGO_URL="$MONGO_URL" MONGO_DB=hrms_regress_e2e "$WORK/hrms" seed -sample) >"$WORK/e2e-seed.log" 2>&1 || return 1
  rm -rf "$WORK/e2e-uploads"
  (cd "$ROOT/backend" && MONGO_URL="$MONGO_URL" MONGO_DB=hrms_regress_e2e PORT=$API_PORT OTP_DEV_ECHO=true STORAGE_DIR="$WORK/e2e-uploads" exec "$WORK/hrms" serve >"$WORK/e2e-api.log" 2>&1) &
  API=$!
  wait_http http://localhost:$API_PORT/api/health || return 1
}
e2e_down() { [ -n "${API:-}" ] && kill "$API" 2>/dev/null; wait "${API:-}" 2>/dev/null; API=""; }

stage_e2e() {
  port_free $API_PORT && port_free $WEB_PORT || return 1
  [ -d "$ROOT/tests/e2e/node_modules" ] || (cd "$ROOT/tests/e2e" && npm install >/dev/null) || return 1
  (cd "$ROOT/frontend" && API_URL=http://localhost:$API_PORT exec npx vite --port $WEB_PORT --strictPort >"$WORK/e2e-web.log" 2>&1) &
  WEB=$!
  ok=0
  e2e_up && wait_http http://localhost:$WEB_PORT || { e2e_down; kill $WEB 2>/dev/null; return 1; }
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT npm run roles) || ok=1
  # search: the people picker's approximate search (typo, initials, keyboard, phone width); it only reads, so it
  # shares the roles seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT SHOTS="$WORK/search-shots" npm run search) || ok=1
  e2e_down
  e2e_up || ok=1
  # forms creates offers, payroll runs and separations, so it gets its own fresh seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT npm run forms) || ok=1
  e2e_down
  e2e_up || ok=1
  # verify: Payroll's joining verification (documents, edits, Excel bulk), also on its own fresh seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT SHOTS="$WORK/verify-shots" npm run verify) || ok=1
  e2e_down
  e2e_up || ok=1
  # advances: an employee's own advance through approval, and Payroll's opening advances upload, on its own fresh seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT SHOTS="$WORK/advances-shots" npm run advances) || ok=1
  # directory: My view's company directory; it only reads, so it shares the advances seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT SHOTS="$WORK/directory-shots" npm run directory) || ok=1
  e2e_down
  e2e_up || ok=1
  # docs: an existing employee uploads in My documents, HR verifies, sends back and extends; on its own fresh seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT SHOTS="$WORK/docs-shots" npm run docs) || ok=1
  e2e_down
  e2e_up || ok=1
  # uniform: sizes on the employee record, issue by size, the sizes summary and the refund basis; on its own fresh seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT SHOTS="$WORK/uniform-shots" npm run uniform) || ok=1
  e2e_down
  e2e_up || ok=1
  # bulkopen: the Bulk update picker, Opening uniform and Opening advances (staff moved onto the HRMS mid-year); own seed.
  (cd "$ROOT/tests/e2e" && HRMS_WEB=http://localhost:$WEB_PORT SHOTS="$WORK/bulkopen-shots" npm run bulkopen) || ok=1
  e2e_down; kill $WEB 2>/dev/null; wait $WEB 2>/dev/null
  mongo_drop hrms_regress_e2e
  return $ok
}

fail=0
for s in $STAGES; do
  case $s in
    go|parity|e2e) note "stage $s"; if "stage_$s"; then RESULTS="$RESULTS: PASS"; else RESULTS="$RESULTS: FAIL"; fail=1; fi ;;
    *) echo "unknown stage $s (go, parity, e2e)" >&2; exit 2 ;;
  esac
done
printf '\n=== regression summary%b\n' "$RESULTS"
[ $fail = 0 ] && echo "ALL PASSED" || echo "SOMETHING FAILED (logs in $WORK)"
exit $fail
