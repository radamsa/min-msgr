# Инфраструктурные константы. Могут быть переопределены: make test-integration TEST_DATABASE_URL='...'
DATABASE_URL          ?= postgres://minmsgr:minmsgr@localhost:55432/minmsgr?sslmode=disable
TEST_DATABASE_URL     ?= postgres://minmsgr:minmsgr@localhost:55432/minmsgr?sslmode=disable
POSTGRES_PORT         ?= 55432

.PHONY: help up down down-clean logs logs-app stats psql image build build-linux test test-integration test-coverage vet fmt run clean

## help: список целей с описанием
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /make /' | column -t -s ':'

## up: собрать образ и поднять весь стек (PostgreSQL + сервис)
up:
	docker compose up -d --build --wait

## down: остановить стек (данные в томах сохраняются)
down:
	docker compose down

## down-clean: остановить стек и удалить тома с данными
down-clean:
	docker compose down -v

## logs: логи PostgreSQL
logs:
	docker compose logs -f postgres

## logs-app: логи сервиса
logs-app:
	docker compose logs -f app

## stats: потребление ресурсов контейнерами (cpu, memory, io)
stats:
	docker compose stats

## psql: интерактивный psql в контейнере
psql:
	docker compose exec postgres psql -U minmsgr -d minmsgr

## image: собрать Docker-образ сервиса
image:
	docker compose build app

## build: собрать сервер и утилиты в ./bin
build:
	go build -o bin/min-msgr ./cmd/min-msgr
	go build -o bin/min-msgr-devkey ./cmd/min-msgr-devkey
	go build -o bin/min-msgr-admin ./cmd/min-msgr-admin

## build-linux: собрать сервер под linux/amd64 в ./bin
build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/min-msgr-linux-amd64 ./cmd/min-msgr

## test: unit-тесты (интеграционные пропускаются без TEST_DATABASE_URL)
test:
	go test ./...

## test-integration: поднять PostgreSQL и прогнать все тесты
test-integration:
	docker compose up -d --wait postgres
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test ./...

## test-coverage: unit-тесты с отчётом покрытия (coverage.html)
test-coverage:
	go test -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

## vet: статический анализ
vet:
	go vet ./...

## fmt: привести Go-файлы к gofmt
fmt:
	gofmt -w .

## run: запустить сервер локально (нужна БД - docker compose up -d --wait postgres)
run:
	DATABASE_URL='$(DATABASE_URL)' go run ./cmd/min-msgr

## clean: удалить собранные бинарники и артефакты тестов
clean:
	rm -rf bin coverage.out coverage.html
