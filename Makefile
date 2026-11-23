DATABASE_URL ?= postgres://visual:visualdev@127.0.0.1:5432/visual?sslmode=disable
DB_TEST_URL ?= $(DATABASE_URL)

.PHONY: dev-db migrate seed-dev api-dev test fmt vet
dev-db:
	docker compose -f deploy/compose.dev.yaml up -d

migrate:
	PGPASSWORD=visualdev psql -h 127.0.0.1 -U visual -d visual -v ON_ERROR_STOP=1 -f migrations/0001_initial.sql

seed-dev:
	PGPASSWORD=visualdev psql -h 127.0.0.1 -U visual -d visual -v ON_ERROR_STOP=1 -f migrations/0002_seed_demo.sql

api-dev:
	APP_ENV=development DEV_AUTH=true DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api -listen 127.0.0.1:8071

test:
	DB_TEST_URL="$(DB_TEST_URL)" go test -race -count=1 ./...

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...
