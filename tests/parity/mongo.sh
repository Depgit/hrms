#!/bin/sh
# Capture from the MongoDB build on the same data as the golden capture: a fresh copy of the parity source database
# imported with `hrms import-pg` into the Mongo database $MONGO_DB (dropped first), the new binary serving it on
# $PORT, capture.mjs into $1, then the server stopped and the Postgres copy dropped. Compare with diff.mjs.
#
#   MONGO_BIN=/path/to/hrms tests/parity/mongo.sh <out-dir>
#
# Environment (defaults in brackets):
#   MONGO_BIN   the MongoDB build of backend/cmd/hrms (required)
#   MONGO_SRC   the source tree it runs from                 [<repo>/backend]
#   MONGO_URL   [mongodb://localhost:27017/?replicaSet=rs0&directConnection=true]
#   MONGO_DB    the throwaway Mongo database                 [hrms_parity]  (never "hrms")
#   SRC_DB      the seeded Postgres source database          [hrms_parity_src]
#   RUN_DB      the Postgres copy import-pg reads            [hrms_parity_run]
#   PORT        API port                                     [8092]
#   PARITY_DIR  dump, known ids, uploads                     [$SP/parity]
set -eu
OUT=${1:?usage: MONGO_BIN=… mongo.sh <out-dir>}
HERE=$(cd "$(dirname "$0")" && pwd)
SP=${SP:-/private/tmp/claude-501/-Users-batman-Documents-backend-HRMS/3d259efb-6a68-4747-b5db-631853d60e6d/scratchpad}
MONGO_BIN=${MONGO_BIN:?set MONGO_BIN to the MongoDB build of the hrms binary}
MONGO_SRC=${MONGO_SRC:-$(cd "$HERE/../../backend" && pwd)}
MONGO_URL=${MONGO_URL:-mongodb://localhost:27017/?replicaSet=rs0&directConnection=true}
MONGO_DB=${MONGO_DB:-hrms_parity}
SRC_DB=${SRC_DB:-hrms_parity_src}
RUN_DB=${RUN_DB:-hrms_parity_run}
PORT=${PORT:-8092}
PARITY_DIR=${PARITY_DIR:-$SP/parity}

[ "$MONGO_DB" = "hrms" ] && { echo "refusing to use the Mongo database hrms" >&2; exit 1; }
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then echo "port $PORT is in use" >&2; exit 1; fi
docker exec hrms-mongo-1 mongosh --quiet --eval "db.getSiblingDB('$MONGO_DB').dropDatabase()" >/dev/null
docker exec hrms-postgres-1 dropdb -U hrms --if-exists "$RUN_DB"
docker exec hrms-postgres-1 createdb -U hrms -T "$SRC_DB" "$RUN_DB"
mkdir -p "$OUT"
(cd "$MONGO_SRC" && MONGO_URL="$MONGO_URL" MONGO_DB="$MONGO_DB" "$MONGO_BIN" import-pg -from "postgres://hrms:hrms@localhost:5433/$RUN_DB" -replace) >"$OUT/import.log" 2>&1 \
  || { echo "import-pg failed, see $OUT/import.log" >&2; docker exec hrms-postgres-1 dropdb -U hrms --if-exists "$RUN_DB"; exit 1; }
docker exec hrms-postgres-1 dropdb -U hrms --if-exists "$RUN_DB"
rm -rf "$PARITY_DIR/uploads-mongo"; mkdir -p "$PARITY_DIR/uploads-mongo"
(cd "$MONGO_SRC" && MONGO_URL="$MONGO_URL" MONGO_DB="$MONGO_DB" PORT="$PORT" OTP_DEV_ECHO=true STORAGE_DIR="$PARITY_DIR/uploads-mongo" \
  exec "$MONGO_BIN" serve >"$OUT/server.log" 2>&1) &
PID=$!
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null' EXIT
i=0; until curl -sf "http://localhost:$PORT/api/health" >/dev/null; do i=$((i+1)); [ $i -gt 60 ] && { echo "API did not start, see $OUT/server.log" >&2; exit 1; }; sleep 0.5; done
sleep 2 # the maintenance sweep runs on start
node "$HERE/capture.mjs" --base "http://localhost:$PORT" --out "$OUT" --known-ids "$PARITY_DIR/known-ids.txt" --api-dir "$MONGO_SRC/internal/api"
