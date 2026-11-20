.PHONY: db migrate seed api web test import regress regress-go regress-parity regress-e2e

MONGO_TEST_URL ?= mongodb://localhost:27017/?replicaSet=rs0&directConnection=true

db:          ## start MongoDB (a one-node replica set; the app uses transactions)
	docker compose up -d --wait mongo

migrate:     ## create the collections, indexes and reference data
	cd backend && go run ./cmd/hrms migrate

seed:        ## reference data + sample organisation
	cd backend && go run ./cmd/hrms seed -sample

api:         ## run the Go API on :8080 (also applies migrations)
	cd backend && go run ./cmd/hrms serve

web:         ## run the React app on :5173 (proxies /api to :8080)
	cd frontend && npm run dev

test:        ## unit tests, plus integration tests against the MongoDB database hrms_test_go
	cd backend && HRMS_TEST_MONGO_URL='$(MONGO_TEST_URL)' go test ./...
	cd frontend && npx tsc -p .

import:      ## copy a PostgreSQL installation into MongoDB: make import [FROM=postgres://…] [REPLACE=1]
	docker compose up -d --wait postgres mongo
	cd backend && go run ./cmd/hrms import-pg $(if $(FROM),-from '$(FROM)') $(if $(REPLACE),-replace)

regress:        ## full regression: Go tests + parity with the Postgres build + browser e2e (tests/regress.sh)
	tests/regress.sh go parity e2e

regress-go:     ## build, vet, gofmt, tsc and every Go test against MongoDB
	tests/regress.sh go

regress-parity: ## the MongoDB build vs the original PostgreSQL build, call for call on identical data
	tests/regress.sh parity

regress-e2e:    ## browser tests: every role and page, then the 74 form flows
	tests/regress.sh e2e
