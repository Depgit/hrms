| What       | How to start                                                  | Port  |
| ---------- | ------------------------------------------------------------- | ----- |
| MongoDB    | `make db` (same as `docker compose up -d --wait mongo`)      | 27017 |
| Go API     | `make api` (applies migrations on start)                      | 8080  |
| React app  | `make web` (forwards `/api` requests to 8080)                 | 5173  |
| PostgreSQL | only to copy an old installation: `make import [FROM=…]`     | 5433  |

Open **[http://localhost:5173](http://localhost:5173/)** in the browser. On a fresh database, run `make seed` once after `make db` to load the reference data and sample employees.

The database is MongoDB (`MONGO_URL`, default `mongodb://localhost:27017/?replicaSet=rs0&directConnection=true`; `MONGO_DB`, default `hrms`). To bring over an existing PostgreSQL installation, `make import` (or `hrms import-pg -from postgres://… [-replace]`) brings that database up to date with the old SQL migrations, copies every table into its collection and checks the counts.
