LOCAL_DATABASE_URL ?= postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable
LOCAL_AMQP_URL     ?= amqp://ledger:ledger@localhost:5672/

.DEFAULT_GOAL := help

.PHONY: help build run test test-integration cover lint fmt tidy up down deps-up deps-down logs docker-build

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-17s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary into ./bin
	CGO_ENABLED=0 go build -trimpath -o bin/ledger ./cmd/ledger

run: deps-up ## Run the service locally against the compose dependencies
	DATABASE_URL='$(LOCAL_DATABASE_URL)' AMQP_URL='$(LOCAL_AMQP_URL)' AMQP_QUEUE=ledger.events.audit LOG_FORMAT=text \
		go run ./cmd/ledger

test: ## Run unit tests
	go test -race -count=1 ./...

test-integration: deps-up ## Run unit and integration tests against PostgreSQL and RabbitMQ
	TEST_DATABASE_URL='$(LOCAL_DATABASE_URL)' TEST_AMQP_URL='$(LOCAL_AMQP_URL)' \
		go test -race -count=1 -tags=integration ./...

cover: deps-up ## Run all tests and print total coverage
	TEST_DATABASE_URL='$(LOCAL_DATABASE_URL)' TEST_AMQP_URL='$(LOCAL_AMQP_URL)' \
		go test -count=1 -tags=integration -coverpkg=./internal/... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format the code
	golangci-lint fmt ./...

tidy: ## Tidy go.mod and go.sum
	go mod tidy

up: ## Build and start the full stack (app, PostgreSQL, RabbitMQ)
	docker compose up --build -d --wait

down: ## Stop the stack and delete its data
	docker compose down -v

deps-up: ## Start only PostgreSQL and RabbitMQ
	docker compose up -d --wait postgres rabbitmq

deps-down: ## Stop PostgreSQL and RabbitMQ
	docker compose stop postgres rabbitmq

logs: ## Follow the application logs
	docker compose logs -f app

docker-build: ## Build the container image
	docker build -t go-ledger-service:local .
