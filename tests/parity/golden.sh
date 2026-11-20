#!/bin/sh
# Golden capture from the ORIGINAL Postgres build: a fresh copy of the parity source database, the baseline binary
# serving it on $PORT, capture.mjs into $1, then the server stopped and the copy dropped.
#
#   tests/parity/golden.sh <out-dir>
#
# Environment (defaults in brackets):
#   PG_BIN      the original binary                         [$SP/pg-baseline/hrms-pg]
#   PG_SRC      its source tree (the server runs from here)  [$SP/pg-baseline/backend]
#   SRC_DB      the seeded source database                   [hrms_parity_src]
#   RUN_DB      the throwaway copy                           [hrms_parity_run]
#   PORT        API port                                     [8092]
#   PARITY_DIR  dump, known ids, uploads                     [$SP/parity]
set -eu
OUT=${1:?usage: golden.sh <out-dir>}
SP=${SP:-/private/tmp/claude-501/-Users-batman-Documents-backend-HRMS/3d259efb-6a68-4747-b5db-631853d60e6d/scratchpad}
PG_BIN=${PG_BIN:-$SP/pg-baseline/hrms-pg}
PG_SRC=${PG_SRC:-$SP/pg-baseline/backend}
SRC_DB=${SRC_DB:-hrms_parity_src}
RUN_DB=${RUN_DB:-hrms_parity_run}
PORT=${PORT:-8092}
PARITY_DIR=${PARITY_DIR:-$SP/parity}
HERE=$(cd "$(dirname "$0")" && pwd)

if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then echo "port $PORT is in use" >&2; exit 1; fi
docker exec hrms-postgres-1 dropdb -U hrms --if-exists "$RUN_DB"
docker exec hrms-postgres-1 createdb -U hrms -T "$SRC_DB" "$RUN_DB"
rm -rf "$PARITY_DIR/uploads-run"; mkdir -p "$PARITY_DIR/uploads-run" "$OUT"
(cd "$PG_SRC" && DATABASE_URL="postgres://hrms:hrms@localhost:5433/$RUN_DB" PORT="$PORT" OTP_DEV_ECHO=true STORAGE_DIR="$PARITY_DIR/uploads-run" \
  exec "$PG_BIN" serve >"$OUT/server.log" 2>&1) &
PID=$!
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null; docker exec hrms-postgres-1 dropdb -U hrms --if-exists "$RUN_DB"' EXIT
i=0; until curl -sf "http://localhost:$PORT/api/health" >/dev/null; do i=$((i+1)); [ $i -gt 60 ] && { echo "API did not start" >&2; exit 1; }; sleep 0.5; done
sleep 2 # the maintenance sweep runs on start
node "$HERE/capture.mjs" --base "http://localhost:$PORT" --out "$OUT" --known-ids "$PARITY_DIR/known-ids.txt" --api-dir "$PG_SRC/internal/api"
